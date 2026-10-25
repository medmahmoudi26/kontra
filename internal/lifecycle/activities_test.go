package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/temporal"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/pgtest"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/temporaltest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	temporaltest.Stop()
	os.Exit(code)
}

var buyer = store.Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000a"), Principal: "key:kk_a_00000001", Via: store.ViaAPIKey}

// pgEnv is one fresh migrated database with a tenant and a direct service.
type pgEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	log  *store.Log
	svc  store.ServiceRef
	acts *LifecycleActivities
}

type resolver struct{ err error }

func (r resolver) BackendFor(context.Context, uuid.UUID, string) (store.Backend, error) {
	if r.err != nil {
		return "", r.err
	}
	return store.BackendDirect, nil
}

type lister struct{ ids []string }

func (l lister) LiveConfigIDs(context.Context, uuid.UUID) ([]string, error) { return l.ids, nil }

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	pool := pgtest.New(t)
	e := &pgEnv{t: t, pool: pool, log: store.NewLog(pool)}
	tenant := uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`, tenant)
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source, timeout_s, retries)
		values ($1, 'hello', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config', 3600, 2)`, tenant)
	e.svc = store.ServiceRef{TenantID: tenant, Tenant: "default", Name: "hello", Kind: "a2a", Timeout: time.Hour, Retries: 2, Retention: 7 * 24 * time.Hour}
	e.acts = &LifecycleActivities{Log: e.log, Backends: resolver{}, Push: lister{ids: []string{"b", "a", "b"}},
		Rules: hooks.KeyRules{OperatorPrefixes: []string{"kontra.escrow."}}}
	return e
}

func (e *pgEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// task creates a task in state with the caller's message in history.
func (e *pgEnv) task(state store.State, meta map[string]any) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), "ctx-1"
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: "ctx-1", Status: a2a.TaskStatus{State: state.A2A()},
		History: []*a2a.Message{msg}, Metadata: meta}
	nt := store.NewTask{ID: id, Service: e.svc, Protocol: store.ProtocolA2A, Owner: buyer, ContextID: "ctx-1",
		State: state, Snapshot: snap}
	if state == store.StateQuoted {
		exp := time.Now().Add(10 * time.Minute)
		nt.QuoteExpiresAt = &exp
	}
	if _, err := e.log.CreateTask(context.Background(), nil, nt); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// status appends a status event at the task's current version.
func (e *pgEnv) status(id uuid.UUID, state a2a.TaskState) {
	e.t.Helper()
	ctx := context.Background()
	row, err := e.log.Load(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	ev := a2a.NewStatusUpdateEvent(row.Snapshot, state, nil)
	next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
	if err != nil {
		e.t.Fatal(err)
	}
	payload, _ := json.Marshal(a2a.StreamResponse{Event: ev})
	st, _ := store.StateFromA2A(state)
	if _, err := e.log.Append(ctx, nil, store.Write{TaskID: id, PrevVersion: row.Version, Kind: store.KindStatus, State: st,
		StatusAt: store.StatusTime(ev.Status.Timestamp), Snapshot: next, Payload: payload}); err != nil {
		e.t.Fatal(err)
	}
}

func appType(err error) string {
	var ae *temporal.ApplicationError
	if errors.As(err, &ae) {
		if !ae.NonRetryable() {
			return ae.Type() + "(retryable)"
		}
		return ae.Type()
	}
	return ""
}

func TestPrepareAndAttemptState(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	id := e.task(store.StateSubmitted, nil)
	row, _ := e.log.Load(ctx, id)

	res, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id.String(), N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Queue != QueueDirect || res.Backend != "direct" || !res.Deadline.Equal(row.Deadline.UTC()) {
		t.Fatalf("prepare %+v, want kontra-direct and the stored deadline %v", res, row.Deadline)
	}
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id.String(), N: 1}); err != nil {
		t.Fatalf("a retried prepare: %v", err)
	}
	st, err := e.acts.AttemptState(ctx, AttemptRef{TaskID: id.String(), N: 1})
	if err != nil || st.Backend != "direct" || st.ReplicaID != "" || st.FirstUpstreamIDAt != nil || st.TaskState != "submitted" {
		t.Fatalf("attempt_state %+v %v", st, err)
	}
	if _, err := e.log.BeginDispatch(ctx, id, 1, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := e.log.SetFirstUpstreamID(ctx, id, 1, "up-1", "upctx"); err != nil {
		t.Fatal(err)
	}
	st, _ = e.acts.AttemptState(ctx, AttemptRef{TaskID: id.String(), N: 1})
	if st.ReplicaID != "r1" || st.FirstUpstreamIDAt == nil || st.Ended {
		t.Fatalf("attempt_state %+v", st)
	}

	// A lost dispatch's attempt is ended by the next prepare.
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id.String(), N: 2, PreviousOutcome: "upstream_unreachable"}); err != nil {
		t.Fatal(err)
	}
	a1, _ := e.log.GetAttempt(ctx, id, 1)
	if a1.EndedAt == nil || a1.Outcome != store.OutcomeUpstreamUnreachable {
		t.Fatalf("attempt 1 %+v, want ended upstream_unreachable", a1)
	}
	if _, err := e.log.GetAttempt(ctx, id, 2); err != nil {
		t.Fatal(err)
	}

	// No backend: service_unavailable, not retried. No task: gone.
	e.acts.Backends = resolver{err: errors.New("unknown service")}
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id.String(), N: 3}); appType(err) != ErrTypeServiceUnavailable {
		t.Errorf("prepare without a backend: %v", err)
	}
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: uuid.Must(uuid.NewV7()).String(), N: 1}); appType(err) != ErrTypeTaskGone {
		t.Errorf("prepare of a missing task: %v", err)
	}
	if _, err := e.acts.AttemptState(ctx, AttemptRef{TaskID: "not-a-uuid", N: 1}); appType(err) != ErrTypeTaskGone {
		t.Errorf("attempt_state of a malformed id: %v", err)
	}
	// The result crosses history: each config is named by its ConfigRef, never its id (which may be the
	// caller's), sorted and unique.
	refs, err := e.acts.ListPushConfigs(ctx, id.String())
	want := []string{ConfigRef(id, "a"), ConfigRef(id, "b")}
	slices.Sort(want)
	if err != nil || !slices.Equal(refs, want) {
		t.Errorf("list_push_configs %v %v, want the refs of a and b, sorted and unique", refs, err)
	}
	for _, r := range refs {
		if r == "a" || r == "b" || !strings.HasPrefix(r, "pc_") {
			t.Errorf("list_push_configs named a config %q", r)
		}
	}
}

// TestConfigRef: a config's ref is stable for (task, id), differs across tasks and ids, and carries
// nothing of the id.
func TestConfigRef(t *testing.T) {
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	const id = "acme-billing-webhook"
	if ConfigRef(a, id) != ConfigRef(a, id) {
		t.Fatal("not stable")
	}
	if ConfigRef(a, id) == ConfigRef(b, id) || ConfigRef(a, id) == ConfigRef(a, id+"x") {
		t.Fatal("refs collide across tasks or ids")
	}
	if r := ConfigRef(a, id); strings.Contains(r, "acme") || strings.Contains(r, a.String()) || len(r) != len("pc_")+64 {
		t.Fatalf("ref %q", r)
	}
}

func TestWriteTerminalEndsTheAttempt(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	id := e.task(store.StateSubmitted, nil)
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id.String(), N: 1}); err != nil {
		t.Fatal(err)
	}
	e.status(id, a2a.TaskStateInputRequired)

	in := WriteTerminalInput{TaskID: id.String(), State: "canceled", Canceled: store.CanceledClient, EndAttempt: string(store.OutcomeCanceled)}
	r, err := e.acts.WriteTerminal(ctx, in)
	if err != nil || r.State != "canceled" || r.Seq == 0 {
		t.Fatalf("write_terminal %+v %v", r, err)
	}
	row, _ := e.log.Load(ctx, id)
	if row.State != store.StateCanceled || row.Snapshot.Metadata[store.MetaCanceled] != "client" {
		t.Fatalf("task %s %v", row.State, row.Snapshot.Metadata)
	}
	a, _ := e.log.GetAttempt(ctx, id, 1)
	if a.EndedAt == nil || a.Outcome != store.OutcomeInterrupted {
		t.Errorf("attempt %+v: a task that ends while waiting at input-required ends its attempt interrupted (D77)", a)
	}
	// Retried: idempotent.
	r2, err := e.acts.WriteTerminal(ctx, in)
	if err != nil || r2 != r {
		t.Errorf("a retried write_terminal %+v %v, want %+v", r2, err, r)
	}
	// Another terminal write to a task that ended: left alone, its state returned.
	r3, err := e.acts.WriteTerminal(ctx, WriteTerminalInput{TaskID: id.String(), State: "failed", Reason: "timeout"})
	if err != nil || r3.State != "canceled" {
		t.Errorf("write_terminal on an ended task %+v %v", r3, err)
	}
	if _, err := e.acts.WriteTerminal(ctx, WriteTerminalInput{TaskID: id.String(), State: "failed", Reason: "nope"}); appType(err) == "" {
		t.Errorf("a reason outside the closed set: %v", err)
	}

	// A timeout while a dispatch ran: the attempt ends timeout.
	id2 := e.task(store.StateSubmitted, nil)
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id2.String(), N: 1}); err != nil {
		t.Fatal(err)
	}
	e.status(id2, a2a.TaskStateWorking)
	if _, err := e.acts.WriteTerminal(ctx, WriteTerminalInput{TaskID: id2.String(), State: "failed", Reason: "timeout", EndAttempt: "timeout"}); err != nil {
		t.Fatal(err)
	}
	row, _ = e.log.Load(ctx, id2)
	a, _ = e.log.GetAttempt(ctx, id2, 1)
	if row.State != store.StateFailed || row.Snapshot.Metadata[store.MetaFailure] != "timeout" || a.Outcome != store.OutcomeTimeout {
		t.Errorf("task %s %v attempt %s", row.State, row.Snapshot.Metadata, a.Outcome)
	}
}

func TestApplyMetadataRefusesAndAudits(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	id := e.task(store.StateCompleted, nil)

	_, err := e.acts.ApplyMetadata(ctx, ApplyMetadataInput{TaskID: id.String(), Source: HookOnTerminal,
		Patch: hooks.Patch{Metadata: map[string]any{"kontra.approve.url": "https://evil"}}})
	if appType(err) != ErrTypePatchRefused {
		t.Fatalf("a key outside the operator prefixes: %v", err)
	}
	var rows int
	var row string
	if err := e.pool.QueryRow(ctx, `select count(*), coalesce(max(convert_from(row_jcs, 'utf8')), '') from kontra.audit`).Scan(&rows, &row); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || !strings.Contains(row, EventPatchRefused) || !strings.Contains(row, "forbidden_key") || strings.Contains(row, "evil") {
		t.Errorf("audit rows %d %s: one row naming the refusal, never the patch", rows, row)
	}
	big := map[string]any{"kontra.escrow.x": strings.Repeat("x", 17<<10)}
	if _, err := e.acts.ApplyMetadata(ctx, ApplyMetadataInput{TaskID: id.String(), Patch: hooks.Patch{Metadata: big}}); appType(err) != ErrTypePatchRefused {
		t.Errorf("a patch over 16 KiB: %v", err)
	}

	seq, err := e.acts.ApplyMetadata(ctx, ApplyMetadataInput{TaskID: id.String(), Source: UpdateAnnotate,
		Patch: hooks.Patch{Metadata: map[string]any{"kontra.escrow.status": "released", hooks.KeyRetainUntil: "2026-11-01T00:00:00Z"}}})
	if err != nil || seq != 2 {
		t.Fatalf("apply_metadata %d %v", seq, err)
	}
	after, _ := e.log.Load(ctx, id)
	if after.Snapshot.Metadata["kontra.escrow.status"] != "released" || after.State != store.StateCompleted {
		t.Errorf("after the patch: %s %v", after.State, after.Snapshot.Metadata)
	}
}

func TestSubmitMovesAQuotedTaskToSubmitted(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	id := e.task(store.StateQuoted, map[string]any{MetaApproveURL: "https://pay.example/t"})
	before := time.Now()
	r, err := e.acts.Submit(ctx, SubmitInput{TaskID: id.String(), Metadata: map[string]any{"kontra.escrow.status": "held"}, TimeoutSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	if r.Deadline.Before(before.Add(590*time.Second)) || r.Deadline.After(time.Now().Add(610*time.Second)) {
		t.Errorf("deadline %v, want about 10 minutes from the approval", r.Deadline)
	}
	row, _ := e.log.Load(ctx, id)
	if row.State != store.StateSubmitted || row.SubmittedAt == nil || row.Snapshot.Metadata["kontra.escrow.status"] != "held" {
		t.Fatalf("task %s %v", row.State, row.Snapshot.Metadata)
	}
	r2, err := e.acts.Submit(ctx, SubmitInput{TaskID: id.String(), TimeoutSeconds: 600})
	if err != nil || !r2.Deadline.Equal(r.Deadline) {
		t.Errorf("a retried submit %+v %v", r2, err)
	}
	var kinds []string
	rows, _ := e.pool.Query(ctx, `select kind from kontra.task_events where task_id = $1 order by seq`, id)
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		kinds = append(kinds, k)
	}
	if strings.Join(kinds, ",") != "task,metadata,status" {
		t.Errorf("events %v, want the approval's metadata then the submitted status", kinds)
	}
	if _, err := e.acts.Submit(ctx, SubmitInput{TaskID: id.String(), Metadata: map[string]any{"x": 1}}); appType(err) != ErrTypePatchRefused {
		t.Errorf("approval metadata outside the prefixes: %v", err)
	}
}

func TestTaskRefCarriesOperatorKeysOnly(t *testing.T) {
	e := newPGEnv(t)
	id := e.task(store.StateSubmitted, map[string]any{"caller": "x", "kontra.escrow.status": "held", hooks.KeyRetainUntil: "t", "kontra.failure": "no"})
	ref, err := e.acts.TaskRef(context.Background(), id.String())
	if err != nil {
		t.Fatal(err)
	}
	if ref.TaskID != id.String() || ref.Kind != "a2a" || ref.Protocol != "a2a" || ref.BuyerID != buyer.BuyerID.String() ||
		ref.Principal != buyer.Principal || ref.Via != "api_key" || ref.ContextID != "ctx-1" || ref.State != "submitted" || ref.Service != "hello" {
		t.Errorf("task ref %+v", ref)
	}
	if len(ref.Metadata) != 2 || ref.Metadata["kontra.escrow.status"] != "held" || ref.Metadata[hooks.KeyRetainUntil] != "t" {
		t.Errorf("task ref metadata %v: operator keys only", ref.Metadata)
	}
}

func TestAppendReviewMessageActivity(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	id := e.task(store.StateCompleted, nil)
	wid := uuid.Must(uuid.NewV7())
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("one more thing"))
	msg.TaskID = a2a.TaskID(id.String())
	if err := e.log.InsertWorkItem(ctx, nil, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute,
		Payload: store.WorkPayload{Request: &a2a.SendMessageRequest{Message: msg}, Owner: buyer}}); err != nil {
		t.Fatal(err)
	}
	seq, err := e.acts.AppendReviewMessage(ctx, ReviewAppendInput{TaskID: id.String(), WorkItemID: wid.String()})
	if err != nil || seq != 2 {
		t.Fatalf("append %d %v", seq, err)
	}
	again, err := e.acts.AppendReviewMessage(ctx, ReviewAppendInput{TaskID: id.String(), WorkItemID: wid.String()})
	if err != nil || again != seq {
		t.Errorf("a retried append %d %v, want the committed seq %d", again, err, seq)
	}
	other := uuid.Must(uuid.NewV7())
	if _, err := e.acts.AppendReviewMessage(ctx, ReviewAppendInput{TaskID: id.String(), WorkItemID: other.String()}); appType(err) != ErrTypeWorkItemGone {
		t.Errorf("a missing work item: %v", err)
	}
}

// write_terminal ends the latest attempt only for the terminal event it wrote itself: a dispatch that
// logged its own terminal event and has not ended its attempt yet (the Dispatcher ends it after the
// handler returns) keeps the attempt for its own outcome.
func TestWriteTerminalEndsOnlyTheAttemptItEnded(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()

	// The race: attempt 1 open, the dispatch logged completed, then the workflow's timeout.
	id := e.task(store.StateSubmitted, nil)
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id.String(), N: 1}); err != nil {
		t.Fatal(err)
	}
	e.status(id, a2a.TaskStateWorking)
	e.status(id, a2a.TaskStateCompleted)
	r, err := e.acts.WriteTerminal(ctx, WriteTerminalInput{TaskID: id.String(), State: "failed", Reason: "timeout", EndAttempt: string(store.OutcomeTimeout)})
	if err != nil || r.State != "completed" {
		t.Fatalf("write_terminal %+v %v, want the task's own completed", r, err)
	}
	a, _ := e.log.GetAttempt(ctx, id, 1)
	if a.EndedAt != nil {
		t.Fatalf("attempt 1 ended %q by a terminal write that wrote nothing; the dispatch's completed is lost", a.Outcome)
	}

	// A retry after its own write committed (the first try failed after it): the attempt is still ended.
	id2 := e.task(store.StateSubmitted, nil)
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id2.String(), N: 1}); err != nil {
		t.Fatal(err)
	}
	e.status(id2, a2a.TaskStateWorking)
	if _, err := e.log.WriteTerminal(ctx, store.TerminalWrite{TaskID: id2, State: store.StateFailed, Reason: store.ReasonTimeout}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.acts.WriteTerminal(ctx, WriteTerminalInput{TaskID: id2.String(), State: "failed", Reason: "timeout", EndAttempt: string(store.OutcomeTimeout)}); err != nil {
		t.Fatal(err)
	}
	if a, _ := e.log.GetAttempt(ctx, id2, 1); a.EndedAt == nil || a.Outcome != store.OutcomeTimeout {
		t.Errorf("attempt %+v: a retried write_terminal ends the attempt its first try wrote the end of", a)
	}

	// Another writer's failure with another reason: not this call's write either.
	id3 := e.task(store.StateSubmitted, nil)
	if _, err := e.acts.Prepare(ctx, PrepareInput{TaskID: id3.String(), N: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.WriteTerminal(ctx, store.TerminalWrite{TaskID: id3, State: store.StateFailed, Reason: store.ReasonUpstreamError, Text: "the upstream refused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.acts.WriteTerminal(ctx, WriteTerminalInput{TaskID: id3.String(), State: "failed", Reason: "timeout", EndAttempt: string(store.OutcomeTimeout)}); err != nil {
		t.Fatal(err)
	}
	if a, _ := e.log.GetAttempt(ctx, id3, 1); a.EndedAt != nil {
		t.Errorf("attempt ended %q by a timeout that lost the race to an upstream_error", a.Outcome)
	}
}
