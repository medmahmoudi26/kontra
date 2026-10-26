package gate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/blob/blobtest"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

// sweepEnv is a migrated database with the team tenant and one direct service.
type sweepEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	log  *store.Log
	svc  store.ServiceRef
}

func newSweepEnv(t *testing.T) *sweepEnv {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	e := &sweepEnv{t: t, pool: pool, log: store.NewLog(pool)}
	tid := uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`, tid)
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source, retention_s) values ($1, 'hello', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config', 3600)`, tid)
	e.svc = store.ServiceRef{TenantID: tid, Tenant: "default", Name: "hello", Kind: "a2a"}
	return e
}

func (e *sweepEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// task creates a task in state (submitted or quoted at creation; any other state is set after), created
// age ago, with one push config.
func (e *sweepEnv) task(state store.State, age time.Duration) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	ctxID := uuid.NewString()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), ctxID
	st := state
	if st != store.StateQuoted {
		st = store.StateSubmitted
	}
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: ctxID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{msg}}
	nt := store.NewTask{ID: id, Service: e.svc, Protocol: store.ProtocolA2A, ContextID: ctxID, State: st, Snapshot: snap,
		Owner: store.Owner{BuyerID: uuid.New(), Principal: "key:kk_a_00000001", Via: store.ViaAPIKey}}
	if st == store.StateQuoted {
		exp := time.Now().Add(time.Hour)
		nt.QuoteExpiresAt = &exp
	}
	if _, err := e.log.CreateTask(context.Background(), nil, nt); err != nil {
		e.t.Fatal(err)
	}
	if state != st {
		e.exec(`update kontra.tasks set state = $2 where id = $1`, id, string(state))
	}
	e.exec(`update kontra.tasks set created_at = now() - $2::bigint * interval '1 microsecond' where id = $1`, id, age.Microseconds())
	e.exec(`insert into kontra.push_configs (task_id, id, config) values ($1, 'pc', '{"url": "https://hook.example/x"}')`, id)
	return id
}

func (e *sweepEnv) row(id uuid.UUID) *store.TaskRow {
	e.t.Helper()
	r, err := e.log.Load(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// workflows is a WorkflowChecker over a set of task ids that have a workflow.
type workflows struct {
	mu     sync.Mutex
	exists map[string]bool
	asked  []string
	err    error
}

func (w *workflows) WorkflowExists(_ context.Context, id a2a.TaskID) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, string(id))
	if w.err != nil {
		return false, w.err
	}
	return w.exists[string(id)], nil
}

// The orphan sweep (spec §14.5, D63): a task in submitted or quoted older than 2 minutes whose workflow does
// not exist ends `failed: lifecycle_unavailable` through the store, its push configs ended in the same
// transaction; a task with a workflow, a younger one and one past submitted are left alone. A budget smaller
// than the candidates is continued by the next pass from its cursor.
func TestOrphanSweep(t *testing.T) {
	e := newSweepEnv(t)
	ctx := context.Background()
	orphan := e.task(store.StateSubmitted, 3*time.Minute)
	quotedOrphan := e.task(store.StateQuoted, 5*time.Minute)
	withWorkflow := e.task(store.StateSubmitted, 10*time.Minute)
	young := e.task(store.StateSubmitted, 30*time.Second)
	working := e.task(store.StateWorking, 10*time.Minute)
	wf := &workflows{exists: map[string]bool{withWorkflow.String(): true}}

	var cursor store.OrphanCandidate
	olderThan := time.Now().Add(-OrphanAge)
	// A budget of one: three passes reach the three candidates, oldest first.
	for pass, want := range []int{0, 1, 1} {
		n, err := SweepOrphans(ctx, e.log, wf, olderThan, &cursor, 1)
		if err != nil || n != want {
			t.Fatalf("pass %d: ended %d, %v (want %d)", pass, n, err, want)
		}
	}
	if len(wf.asked) != 3 || wf.asked[0] != withWorkflow.String() || wf.asked[1] != quotedOrphan.String() || wf.asked[2] != orphan.String() {
		t.Fatalf("asked %v", wf.asked)
	}
	for _, id := range []uuid.UUID{orphan, quotedOrphan} {
		r := e.row(id)
		if r.State != store.StateFailed || r.Snapshot.Metadata[store.MetaFailure] != string(store.ReasonLifecycleUnavailable) ||
			r.Snapshot.Status.Message == nil || r.TerminalAt == nil || r.RetentionUntil == nil {
			t.Fatalf("orphan %s: %s %v", id, r.State, r.Snapshot.Metadata)
		}
		evs, err := e.log.Events(ctx, id, 0, 100)
		if err != nil || len(evs) != 2 || evs[1].Kind != store.KindStatus {
			t.Fatalf("orphan %s log: %+v %v", id, evs, err)
		}
		if !scalar[bool](t, e.pool, `select failed_at is not null from kontra.push_configs where task_id = $1`, id) {
			t.Fatalf("orphan %s: its push config is still delivering", id)
		}
	}
	for _, id := range []uuid.UUID{withWorkflow, young} {
		if r := e.row(id); r.State != store.StateSubmitted {
			t.Fatalf("task %s is %s", id, r.State)
		}
	}
	if r := e.row(working); r.State != store.StateWorking {
		t.Fatalf("a working task is %s", r.State)
	}
	if scalar[bool](t, e.pool, `select failed_at is not null from kontra.push_configs where task_id = $1`, withWorkflow) {
		t.Fatal("a task with a workflow had its push config ended")
	}
	// The next pass reaches the end and starts over; nothing is left to end.
	wf.asked = nil
	n, err := SweepOrphans(ctx, e.log, wf, olderThan, &cursor, OrphanBudget)
	if err != nil || n != 0 || cursor != (store.OrphanCandidate{}) || len(wf.asked) != 0 {
		t.Fatalf("the pass after the last candidate: %d %v %+v asked %v", n, err, cursor, wf.asked)
	}
	n, err = SweepOrphans(ctx, e.log, wf, olderThan, &cursor, OrphanBudget)
	if err != nil || n != 0 || len(wf.asked) != 1 || wf.asked[0] != withWorkflow.String() {
		t.Fatalf("a fresh pass: %d %v asked %v", n, err, wf.asked)
	}
	// FailOrphan of a task that moved on meanwhile writes nothing.
	if ok, err := e.log.FailOrphan(ctx, working); ok || err != nil {
		t.Fatalf("FailOrphan of a working task: %v %v", ok, err)
	}
}

// A sweep that cannot ask Temporal fails nothing: a task is never ended because its workflow could not be
// looked up.
func TestOrphanSweepNeverFailsWhatItCannotCheck(t *testing.T) {
	e := newSweepEnv(t)
	orphan := e.task(store.StateSubmitted, 3*time.Minute)
	wf := &workflows{err: errors.New("temporal: connection refused")}
	var cursor store.OrphanCandidate
	n, err := SweepOrphans(context.Background(), e.log, wf, time.Now().Add(-OrphanAge), &cursor, OrphanBudget)
	if err == nil || n != 0 {
		t.Fatalf("sweep with Temporal down: %d %v", n, err)
	}
	if r := e.row(orphan); r.State != store.StateSubmitted {
		t.Fatalf("the orphan is %s", r.State)
	}
}

// The retention sweep (spec §5.4): a terminal task past retention_until goes with its task_events,
// attempts, work_items and push_configs; one within retention and an open one stay. MCP sessions closed for
// longer than their service's retention (or the default, for the gate's own /mcp) go too.
func TestRetentionSweep(t *testing.T) {
	e := newSweepEnv(t)
	ctx := context.Background()
	expired := e.task(store.StateSubmitted, time.Hour)
	kept := e.task(store.StateSubmitted, time.Hour)
	open := e.task(store.StateSubmitted, time.Hour)
	for _, id := range []uuid.UUID{expired, kept} {
		if err := e.log.BeginAttempt(ctx, store.Attempt{TaskID: id, N: 1, Backend: store.BackendDirect}); err != nil {
			t.Fatal(err)
		}
		e.exec(`insert into kontra.work_items (id, task_id, kind, payload) values ($1, $2, 'execute', '{}')`, uuid.Must(uuid.NewV7()), id)
		if _, err := e.log.WriteTerminal(ctx, store.TerminalWrite{TaskID: id, State: store.StateCompleted, Text: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	e.exec(`update kontra.tasks set retention_until = now() - interval '1 minute' where id = $1`, expired)
	if r := e.row(kept); r.RetentionUntil == nil || time.Until(*r.RetentionUntil) < 50*time.Minute {
		t.Fatalf("retention_until of a completed task: %v (the service keeps 1 h)", r.RetentionUntil)
	}
	sid := func() []byte { b := uuid.New(); return append(b[:], b[:]...) }
	oldGate, freshGate, oldSvc, openSess := sid(), sid(), sid(), sid()
	e.exec(`insert into kontra.mcp_sessions (client_sid_hash, buyer_id, principal, via, protocol_version, closed_at, close_reason)
		values ($1, $2, 'anonymous', 'anonymous', '2025-11-25', now() - interval '8 days', 'idle'),
		       ($3, $2, 'anonymous', 'anonymous', '2025-11-25', now() - interval '1 day', 'idle')`, oldGate, uuid.New(), freshGate)
	e.exec(`insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via, protocol_version, closed_at, close_reason)
		values ($1, $2, 'hello', $3, 'anonymous', 'anonymous', '2025-11-25', now() - interval '2 hours', 'deleted'),
		       ($4, $2, 'hello', $3, 'anonymous', 'anonymous', '2025-11-25', null, null)`, oldSvc, e.svc.TenantID, uuid.New(), openSess)

	n, err := SweepRetention(ctx, e.log, time.Now(), nil)
	if err != nil || n != 1 {
		t.Fatalf("retention sweep: %d %v", n, err)
	}
	for table, col := range map[string]string{"tasks": "id", "task_events": "task_id", "attempts": "task_id", "work_items": "task_id", "push_configs": "task_id"} {
		if c := scalar[int](t, e.pool, `select count(*) from kontra.`+table+` where `+col+` = $1`, expired); c != 0 {
			t.Errorf("%s still holds %d rows of the expired task", table, c)
		}
		if c := scalar[int](t, e.pool, `select count(*) from kontra.`+table+` where `+col+` = $1`, kept); c == 0 {
			t.Errorf("%s lost the rows of a task within retention", table)
		}
	}
	if r := e.row(open); r.State != store.StateSubmitted {
		t.Fatalf("an open task: %s", r.State)
	}
	if c := scalar[int](t, e.pool, `select count(*) from kontra.mcp_sessions`); c != 2 {
		t.Fatalf("MCP sessions left: %d, want 2", c)
	}
	if c := scalar[int](t, e.pool, `select count(*) from kontra.mcp_sessions where client_sid_hash = any($1)`, [][]byte{freshGate, openSess}); c != 2 {
		t.Fatalf("a session within retention or still open was deleted (%d left)", c)
	}
	// Nothing left to delete.
	if n, err := SweepRetention(ctx, e.log, time.Now(), nil); err != nil || n != 0 {
		t.Fatalf("a second pass: %d %v", n, err)
	}
}

// The orphan sweep asks the gate's own Temporal namespace (lifecycle.Client.WorkflowExists, spec §14.5): a
// task whose task-<id> workflow exists is left alone, one without is ended lifecycle_unavailable.
func TestOrphanSweepAsksTemporal(t *testing.T) {
	e := newSweepEnv(t)
	c := temporaltest.Client(t)
	ctx := context.Background()
	orphan := e.task(store.StateSubmitted, 3*time.Minute)
	started := e.task(store.StateSubmitted, 3*time.Minute)
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: lifecycle.WorkflowID(a2a.TaskID(started.String())), TaskQueue: "nobody-polls-this"}, "NotRegistered")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.TerminateWorkflow(context.Background(), run.GetID(), run.GetRunID(), "test over") })
	var cursor store.OrphanCandidate
	n, err := SweepOrphans(ctx, e.log, lifecycle.NewClient(c), time.Now().Add(-OrphanAge), &cursor, OrphanBudget)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if r := e.row(orphan); r.State != store.StateFailed || r.Snapshot.Metadata[store.MetaFailure] != string(store.ReasonLifecycleUnavailable) {
		t.Fatalf("the orphan: %s %v", r.State, r.Snapshot.Metadata)
	}
	if r := e.row(started); r.State != store.StateSubmitted {
		t.Fatalf("a task whose workflow exists: %s", r.State)
	}
}

// The overlap sweep revokes a rotated node key once its 24 h overlap has passed (spec/tunnel.md §12), with
// its audit row, and leaves a key still inside its overlap and an active key alone.
func TestOverlapSweep(t *testing.T) {
	e := newSweepEnv(t)
	ctx := context.Background()
	key := func(n byte) (string, []byte) {
		pub := make([]byte, 32)
		pub[0] = n
		return "k1_" + strings.Repeat(string(rune('a'+n)), 26), pub
	}
	tid := e.svc.TenantID
	due, duePub := key(1)
	inOverlap, inPub := key(2)
	active, activePub := key(3)
	for _, k := range []struct {
		id  string
		pub []byte
	}{{active, activePub}, {due, duePub}, {inOverlap, inPub}} {
		e.exec(`insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ($1, $2, $3, 'n')`, k.id, tid, k.pub)
	}
	e.exec(`update kontra.tunnel_keys set revoked_reason = 'rotated', rotated_to = $2, overlap_until = now() - interval '1 second' where key_id = $1`, due, active)
	e.exec(`update kontra.tunnel_keys set revoked_reason = 'rotated', rotated_to = $2, overlap_until = now() + interval '1 hour' where key_id = $1`, inOverlap, active)

	keys := enroll.NewStore(e.pool, enroll.Options{})
	ended, err := SweepOverlaps(ctx, keys)
	if err != nil || len(ended) != 1 || ended[0] != due {
		t.Fatalf("SweepOverlaps = %v %v", ended, err)
	}
	var revoked []string
	rows, err := e.pool.Query(ctx, `select key_id from kontra.tunnel_keys where revoked_at is not null order by key_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		revoked = append(revoked, id)
	}
	if len(revoked) != 1 || revoked[0] != due {
		t.Errorf("revoked %v, want only %s", revoked, due)
	}
	if rows, err := audit.Verify(ctx, e.pool); err != nil || len(rows) != 1 {
		t.Errorf("audit: %d rows, %v", len(rows), err)
	}
	if ended, err := SweepOverlaps(ctx, keys); err != nil || len(ended) != 0 {
		t.Errorf("a second pass: %v %v", ended, err)
	}
}

// orderedDeleter is the bucket as the retention sweep sees it, recording for each DeleteTask whether the task's
// row still existed when its objects were deleted.
type orderedDeleter struct {
	store.ObjectDeleter
	pool *pgxpool.Pool
	fail error
	mu   sync.Mutex
	seen map[uuid.UUID]bool // task -> its row existed at the call
}

func (d *orderedDeleter) DeleteTask(ctx context.Context, tenant, task uuid.UUID) error {
	var exists bool
	if err := d.pool.QueryRow(ctx, `select exists (select 1 from kontra.tasks where id = $1)`, task).Scan(&exists); err != nil {
		return err
	}
	d.mu.Lock()
	d.seen[task] = exists
	d.mu.Unlock()
	if d.fail != nil {
		return d.fail
	}
	return d.ObjectDeleter.DeleteTask(ctx, tenant, task)
}

// Retention with a bucket (spec §5.4, D167): a task's objects go first, by prefix, then its rows (its blobs rows
// with them); a task within retention keeps its objects. A bucket that refuses the delete keeps the task's rows,
// so the next pass tries again.
func TestRetentionDeletesObjectsBeforeRows(t *testing.T) {
	e := newSweepEnv(t)
	ctx := context.Background()
	bt := blobtest.New(t, "ret")
	expired := e.task(store.StateSubmitted, time.Hour)
	kept := e.task(store.StateSubmitted, time.Hour)
	for _, id := range []uuid.UUID{expired, kept} {
		_, row, err := blob.Externalize(ctx, bt.Store, e.svc.TenantID, id, &a2a.Part{Content: a2a.Raw(make([]byte, 300<<10))})
		if err != nil {
			t.Fatal(err)
		}
		if err := blob.Insert(ctx, e.pool, []blob.Row{row}); err != nil {
			t.Fatal(err)
		}
		// An object without a row (stored for an event that was then refused) goes with its task too.
		ref, _ := blob.NewRef()
		if err := bt.Store.Put(ctx, blob.Key{Tenant: e.svc.TenantID, Task: id, Ref: ref}, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
		if _, err := e.log.WriteTerminal(ctx, store.TerminalWrite{TaskID: id, State: store.StateCompleted, Text: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	e.exec(`update kontra.tasks set retention_until = now() - interval '1 minute' where id = $1`, expired)

	// The bucket refuses: nothing is deleted, and the error is reported.
	failing := &orderedDeleter{ObjectDeleter: bt.Store, pool: e.pool, fail: errors.New("bucket down"), seen: map[uuid.UUID]bool{}}
	if n, err := SweepRetention(ctx, e.log, time.Now(), failing); err == nil || n != 0 {
		t.Fatalf("a sweep whose bucket refuses: %d %v", n, err)
	}
	if c := scalar[int](t, e.pool, `select count(*) from kontra.tasks where id = $1`, expired); c != 1 || len(bt.Keys(t)) != 4 {
		t.Fatalf("a refused object delete removed the task (%d) or objects (%v)", c, bt.Keys(t))
	}

	d := &orderedDeleter{ObjectDeleter: bt.Store, pool: e.pool, seen: map[uuid.UUID]bool{}}
	n, err := SweepRetention(ctx, e.log, time.Now(), d)
	if err != nil || n != 1 {
		t.Fatalf("retention sweep: %d %v", n, err)
	}
	if existed, called := d.seen[expired]; !called || !existed {
		t.Fatalf("DeleteTask for the expired task: called %v, its row existed %v (want the objects deleted first)", called, existed)
	}
	if _, called := d.seen[kept]; called {
		t.Error("the objects of a task within retention were deleted")
	}
	for _, k := range bt.Keys(t) {
		if strings.Contains(k, expired.String()) {
			t.Errorf("object %s of the expired task is left", k)
		}
	}
	if left := bt.Keys(t); len(left) != 2 {
		t.Errorf("objects left %v, want the kept task's two", left)
	}
	if c := scalar[int](t, e.pool, `select count(*) from kontra.blobs where task_id = $1`, expired); c != 0 {
		t.Errorf("%d blobs rows of the expired task left", c)
	}
	if c := scalar[int](t, e.pool, `select count(*) from kontra.blobs where task_id = $1`, kept); c != 1 {
		t.Errorf("the kept task has %d blobs rows", c)
	}
}

// pickyDeleter is a bucket that refuses to delete one task's objects (an object lock, an access-denied prefix)
// and counts the calls for each task.
type pickyDeleter struct {
	store.ObjectDeleter
	fail  uuid.UUID
	mu    sync.Mutex
	calls map[uuid.UUID]int
}

func (d *pickyDeleter) DeleteTask(ctx context.Context, tenant, task uuid.UUID) error {
	d.mu.Lock()
	d.calls[task]++
	d.mu.Unlock()
	if task == d.fail {
		return errors.New("access denied")
	}
	return d.ObjectDeleter.DeleteTask(ctx, tenant, task)
}

// One task whose objects cannot be deleted does not hold retention back (spec §5.4): it keeps its rows, is not
// tried again in the same pass, the expired tasks after it are deleted batch after batch, closed MCP sessions are
// still swept, and the pass reports the failure without ending early.
func TestRetentionSurvivesAFailingTask(t *testing.T) {
	e := newSweepEnv(t)
	ctx := context.Background()
	bt := blobtest.New(t, "")
	var ids []uuid.UUID
	for i := range 3 {
		id := e.task(store.StateSubmitted, time.Hour)
		_, row, err := blob.Externalize(ctx, bt.Store, e.svc.TenantID, id, &a2a.Part{Content: a2a.Raw(make([]byte, 300<<10))})
		if err != nil {
			t.Fatal(err)
		}
		if err := blob.Insert(ctx, e.pool, []blob.Row{row}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.log.WriteTerminal(ctx, store.TerminalWrite{TaskID: id, State: store.StateCompleted, Text: "done"}); err != nil {
			t.Fatal(err)
		}
		// The failing task's retention_until is the oldest: it heads every query.
		e.exec(`update kontra.tasks set retention_until = now() - $2::bigint * interval '1 minute' where id = $1`, id, 10-i)
		ids = append(ids, id)
	}
	old := uuid.New()
	e.exec(`insert into kontra.mcp_sessions (client_sid_hash, buyer_id, principal, via, protocol_version, closed_at, close_reason)
		values ($1, $2, 'anonymous', 'anonymous', '2025-11-25', now() - interval '8 days', 'idle')`, append(old[:], old[:]...), uuid.New())

	d := &pickyDeleter{ObjectDeleter: bt.Store, fail: ids[0], calls: map[uuid.UUID]int{}}
	n, err := retentionPass(ctx, e.log, time.Now(), d, 2, 10)
	if err == nil || !strings.Contains(err.Error(), ids[0].String()) {
		t.Errorf("the pass did not report the failing task: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d tasks, want the two after the failing one", n)
	}
	if c := scalar[int](t, e.pool, `select count(*) from kontra.tasks where id = $1`, ids[0]); c != 1 {
		t.Error("the task whose objects could not be deleted lost its rows")
	}
	if c := scalar[int](t, e.pool, `select count(*) from kontra.tasks where id = any($1)`, ids[1:]); c != 0 {
		t.Errorf("%d expired tasks after the failing one were kept", c)
	}
	if d.calls[ids[0]] != 1 {
		t.Errorf("the failing task was tried %d times in one pass", d.calls[ids[0]])
	}
	if c := scalar[int](t, e.pool, `select count(*) from kontra.mcp_sessions`); c != 0 {
		t.Error("a closed MCP session past retention was not swept")
	}
	if keys := bt.Keys(t); len(keys) != 1 || !strings.Contains(keys[0], ids[0].String()) {
		t.Errorf("objects left %v, want only the failing task's", keys)
	}
}
