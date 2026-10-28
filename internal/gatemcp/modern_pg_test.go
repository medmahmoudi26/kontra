package gatemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tenant"
)

// replyRecorder is a Hirer whose Reply records each follow-up and, as the task's dispatch would, moves the task out
// of input-required (a status row: working); err, when set, refuses the follow-up instead.
type replyRecorder struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	got  []string // "<task>:<text>"
	err  error
}

func (r *replyRecorder) Hire(context.Context, HireRequest) (HireOutcome, error) {
	return HireOutcome{}, errors.New("not in this test")
}

func (r *replyRecorder) Reply(ctx context.Context, p auth.Principal, task *store.TaskRow, parts a2a.ContentParts) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	if task.Owner.BuyerID != p.BuyerID {
		return 0, a2a.ErrTaskNotFound
	}
	r.got = append(r.got, task.ID.String()+":"+parts[0].Text())
	_, err := r.pool.Exec(ctx, `with e as (insert into kontra.task_events (task_id, seq, kind, payload)
			select id, version + 1, 'status', '{}' from kontra.tasks where id = $1 returning seq)
		update kontra.tasks set state = 'working', version = (select seq from e) where id = $1`, task.ID)
	return task.Version, err
}

func (r *replyRecorder) replies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// cancelRecorder is the lifecycle's MCP-mode cancel, recorded; block, when set, holds every call until closed.
type cancelRecorder struct {
	mu    sync.Mutex
	ids   []string
	block chan struct{}
}

func (c *cancelRecorder) CancelMCP(ctx context.Context, id a2a.TaskID) (lifecycle.CancelResult, bool, error) {
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return lifecycle.CancelResult{}, false, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, string(id))
	return lifecycle.CancelResult{}, true, nil
}

func (c *cancelRecorder) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ids...)
}

type noWake struct{}

func (noWake) WaitTask(context.Context, uuid.UUID) (<-chan struct{}, func()) { return nil, func() {} }

type modernEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	team   uuid.UUID
	tasks  *HireTasks
	hirer  *replyRecorder
	cancel *cancelRecorder
	buyer  auth.Principal
}

func newModernEnv(t *testing.T) *modernEnv {
	t.Helper()
	e := &modernEnv{t: t, pool: pgtest.New(t)}
	ctx := context.Background()
	var err error
	if e.team, err = tenant.EnsureTeam(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, allow, source)
		values ($1, 'hello', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}'::jsonb, '{org:default}', 'db')`, e.team); err != nil {
		t.Fatal(err)
	}
	e.hirer = &replyRecorder{pool: e.pool}
	e.cancel = &cancelRecorder{}
	e.tasks, err = NewHireTasks(HireTasksConfig{Log: store.NewLog(e.pool), Waker: noWake{}, Lifecycle: e.cancel, Hirer: e.hirer,
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	e.buyer = keyPrincipal(e.team, "default", "kk_buyer")
	return e
}

// task inserts a hire task of the buyer in state with snapshot task, its log a task row and then one status row per
// entry of statuses (the last one's seq is the task's version); hired marks it as hire's.
func (e *modernEnv) task(buyer uuid.UUID, state store.State, task *a2a.Task, statuses int, hired bool) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	task.ID = a2a.TaskID(id.String())
	snap, err := json.Marshal(task)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx := context.Background()
	version := 1 + statuses
	_, err = e.pool.Exec(ctx, `insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id,
			state, version, snapshot, status_at, quote_expires_at, submitted_at, terminal_at, hired)
		values ($1, $2, 'hello', 'a2a', $3, 'key:kk_buyer', 'api_key', 'ctx', $4, $5, $6::json, now(),
			case when $4 = 'quoted' then now() + interval '10 minutes' end,
			case when $4 <> 'quoted' then now() end,
			case when $4 in ('completed', 'failed', 'canceled', 'rejected') then now() end, $7)`,
		id, e.team, buyer, string(state), version, string(snap), hired)
	if err != nil {
		e.t.Fatal(err)
	}
	for seq := 1; seq <= version; seq++ {
		kind := "status"
		if seq == 1 {
			kind = "task"
		}
		if _, err := e.pool.Exec(ctx, `insert into kontra.task_events (task_id, seq, kind, payload) values ($1, $2, $3, '{}')`, id, seq, kind); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

func (e *modernEnv) state(id uuid.UUID) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `select state from kontra.tasks where id = $1`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// update sends a modern tasks/update with inputResponses (raw JSON) about task.
func (e *modernEnv) update(rd Reader, task uuid.UUID, inputResponses string) (json.RawMessage, *mcpwire.Error, error) {
	params := `{"taskId":"` + task.String() + `"`
	if inputResponses != "" {
		params += `,"inputResponses":` + inputResponses
	}
	return e.tasks.Update(context.Background(), rd, json.RawMessage(params+"}"))
}

func accept(reply string) string {
	return `{"action":"accept","content":{"reply":` + fmt.Sprintf("%q", reply) + `}}`
}

// A modern tasks/update (§12.4, §12.5, DM4-7; the corrections' WP-05 rule): the one answerable key is the gate id of
// the question outstanding now, as the reader's own request sees it in tasks/get; its accept {reply} is the §7
// follow-up, once. A quoted task (D38, D45), a task asking nothing, an earlier turn's key, another task's key, a
// malformed key, a decline, an accept without a string reply, a question the reader did not declare form elicitation
// for, and a question that is not text only are all acknowledged and forward nothing. Another buyer's task is
// -32602, as an unknown one; inputResponses that is not an object is -32602; a follow-up the lifecycle did not accept
// in time is the lifecycle-unavailable error (503).
func TestModernUpdate(t *testing.T) {
	e := newModernEnv(t)
	form := Reader{Principal: e.buyer, Era: mcpwire.EraModern, FormElicitation: true}
	noForm := Reader{Principal: e.buyer, Era: mcpwire.EraModern}
	quoted := e.task(e.buyer.BuyerID, store.StateQuoted,
		taskWith(a2a.TaskStateInputRequired, nil, map[string]any{lifecycle.MetaApproveURL: "https://app.kontra.sh/#/t/q"}), 1, true)
	asking := e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil), 2, true)
	dataQ := e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired,
		&a2a.Message{ID: "m", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewDataPart(map[string]any{"pick": "date"})}}, nil), 2, true)
	working := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 1, true)
	key := func(id uuid.UUID, seq int64) string { return store.GateRequestID(id, seq) }

	ignored := []struct {
		name string
		rd   Reader
		task uuid.UUID
		resp string
	}{
		{"the quoted event's key", form, quoted, `{"` + key(quoted, 2) + `":` + accept("I accept") + `}`},
		{"the quoted task's first key", form, quoted, `{"` + key(quoted, 1) + `":` + accept("yes") + `}`},
		{"an accept with no content on a quoted task", form, quoted, `{"` + key(quoted, 2) + `":{"action":"accept"}}`},
		{"another task's key on a quoted task", form, quoted, `{"` + key(asking, 3) + `":` + accept("x") + `}`},
		{"an earlier turn's key", form, asking, `{"` + key(asking, 2) + `":` + accept("friday") + `}`},
		{"another task's key", form, asking, `{"` + key(dataQ, 3) + `":` + accept("friday") + `}`},
		{"a malformed key", form, asking, `{"k.` + asking.String() + `.03":` + accept("friday") + `,"x":` + accept("friday") + `}`},
		{"a decline", form, asking, `{"` + key(asking, 3) + `":{"action":"decline"}}`},
		{"a cancel", form, asking, `{"` + key(asking, 3) + `":{"action":"cancel"}}`},
		{"a reply that is not a string", form, asking, `{"` + key(asking, 3) + `":{"action":"accept","content":{"reply":5}}}`},
		{"another member", form, asking, `{"` + key(asking, 3) + `":{"action":"accept","content":{"answer":"friday"}}}`},
		{"an empty reply", form, asking, `{"` + key(asking, 3) + `":` + accept("") + `}`},
		{"no form elicitation declared", noForm, asking, `{"` + key(asking, 3) + `":` + accept("friday") + `}`},
		{"a question with a data part", form, dataQ, `{"` + key(dataQ, 3) + `":` + accept("friday") + `}`},
		{"a task asking nothing", form, working, `{"` + key(working, 2) + `":` + accept("friday") + `}`},
		{"no answers", form, asking, `{}`},
	}
	for _, tc := range ignored {
		res, rpcErr, err := e.update(tc.rd, tc.task, tc.resp)
		if err != nil || rpcErr != nil || string(res) != `{"resultType":"complete"}` {
			t.Errorf("%s: %s %v %v", tc.name, res, rpcErr, err)
		}
	}
	if got := e.hirer.replies(); len(got) != 0 {
		t.Fatalf("ignored answers were sent: %v", got)
	}
	if e.state(quoted) != "quoted" || e.state(asking) != "input-required" {
		t.Fatalf("after the ignored answers: quoted %s, asking %s", e.state(quoted), e.state(asking))
	}

	// Another buyer's task, an unknown one, a non-object inputResponses.
	other := Reader{Principal: keyPrincipal(e.team, "default", "kk_other"), Era: mcpwire.EraModern, FormElicitation: true}
	for name, call := range map[string]func() (json.RawMessage, *mcpwire.Error, error){
		"another buyer": func() (json.RawMessage, *mcpwire.Error, error) {
			return e.update(other, asking, `{"`+key(asking, 3)+`":`+accept("x")+`}`)
		},
		"an unknown task": func() (json.RawMessage, *mcpwire.Error, error) {
			return e.update(form, uuid.Must(uuid.NewV7()), `{}`)
		},
		"an unhired task": func() (json.RawMessage, *mcpwire.Error, error) {
			return e.update(form, e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("?"), nil), 1, false), `{}`)
		},
		"inputResponses an array": func() (json.RawMessage, *mcpwire.Error, error) { return e.update(form, asking, `[]`) },
		"no inputResponses":       func() (json.RawMessage, *mcpwire.Error, error) { return e.update(form, asking, ``) },
	} {
		res, rpcErr, err := call()
		if err != nil || rpcErr == nil || rpcErr.Code != mcpwire.CodeInvalidParams || res != nil {
			t.Errorf("%s: %s %v %v", name, res, rpcErr, err)
		}
	}

	// A lifecycle that does not accept the follow-up in time: 503, so the client answers again; nothing taken.
	e.hirer.err = fmt.Errorf("not accepted: %w", lifecycle.ErrUnavailable)
	if _, _, err := e.update(form, asking, `{"`+key(asking, 3)+`":`+accept("friday")+`}`); !errors.Is(err, errLifecycleUnavailable) {
		t.Errorf("a follow-up not accepted in time: %v", err)
	}
	// A refused follow-up (another follow-up of the turn was taken first): acknowledged, as an answer not outstanding.
	e.hirer.err = a2a.NewError(a2a.ErrUnsupportedOperation, "a follow-up of this turn was accepted first")
	if res, rpcErr, err := e.update(form, asking, `{"`+key(asking, 3)+`":`+accept("friday")+`}`); err != nil || rpcErr != nil || string(res) != `{"resultType":"complete"}` {
		t.Errorf("a refused follow-up: %s %v %v", res, rpcErr, err)
	}
	e.hirer.err = nil

	// The outstanding key, accepted: the follow-up, once; the second answer to it is ignored.
	for range 2 {
		res, rpcErr, err := e.update(form, asking, `{"`+key(asking, 3)+`":`+accept("friday")+`,"`+key(asking, 2)+`":`+accept("monday")+`}`)
		if err != nil || rpcErr != nil || string(res) != `{"resultType":"complete"}` {
			t.Fatalf("the answer: %s %v %v", res, rpcErr, err)
		}
	}
	if got := e.hirer.replies(); !reflect.DeepEqual(got, []string{asking.String() + ":friday"}) {
		t.Fatalf("follow-ups sent: %v, want friday once", got)
	}
}

// A modern tasks/get of a hire task (§12.5, §11.12's table, DM4-7): resultType complete, ttlMs and pollIntervalMs (not
// 2025-11-25's ttl and pollInterval); quoted is working with the approve URL in statusMessage and _meta; a text-only
// question is input_required with exactly one inputRequests entry, a form elicitation/create keyed by the gate id of
// the event that asked, for a reader whose request declared form elicitation, and working with the reply-tool text
// otherwise; an ended task carries its rendered CallToolResult (resultType complete) as result, failed as completed
// with isError and sh.kontra/failure; canceled is cancelled with no result; another buyer's is -32602.
func TestModernGet(t *testing.T) {
	e := newModernEnv(t)
	form := Reader{Principal: e.buyer, Era: mcpwire.EraModern, FormElicitation: true}
	get := func(rd Reader, id uuid.UUID) map[string]any {
		t.Helper()
		res, rpcErr, err := e.tasks.Get(context.Background(), rd, id)
		if err != nil || rpcErr != nil {
			t.Fatalf("tasks/get %s: %v %v", id, rpcErr, err)
		}
		var o map[string]any
		if err := json.Unmarshal(res, &o); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"ttl", "pollInterval", "task"} {
			if _, ok := o[k]; ok {
				t.Errorf("tasks/get %s carries the 2025-11-25 member %s: %s", id, k, res)
			}
		}
		if o["resultType"] != "complete" || o["taskId"] != id.String() || o["pollIntervalMs"] != float64(1000) || o["ttlMs"] == nil ||
			o["createdAt"] == nil || o["lastUpdatedAt"] == nil {
			t.Errorf("tasks/get %s: %s", id, res)
		}
		return o
	}

	quoted := e.task(e.buyer.BuyerID, store.StateQuoted,
		taskWith(a2a.TaskStateInputRequired, nil, map[string]any{lifecycle.MetaApproveURL: "https://app.kontra.sh/#/t/q"}), 1, true)
	if o := get(form, quoted); o["status"] != "working" || o["statusMessage"] != "https://app.kontra.sh/#/t/q" ||
		!reflect.DeepEqual(o["_meta"], map[string]any{MetaApproveURL: "https://app.kontra.sh/#/t/q"}) || o["inputRequests"] != nil {
		t.Errorf("a quoted task: %v", o)
	}

	asking := e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil), 4, true)
	o := get(form, asking)
	want := map[string]any{store.GateRequestID(asking, 5): map[string]any{"method": "elicitation/create", "params": map[string]any{
		"mode": "form", "message": "Which date?", "requestedSchema": map[string]any{"type": "object",
			"properties": map[string]any{"reply": map[string]any{"type": "string"}}, "required": []any{"reply"}}}}}
	if o["status"] != "input_required" || o["statusMessage"] != "Which date?" || !reflect.DeepEqual(o["inputRequests"], want) || o["_meta"] != nil {
		t.Errorf("a question to a reader that declared form elicitation: %v", o)
	}
	o = get(Reader{Principal: e.buyer, Era: mcpwire.EraModern}, asking)
	if o["status"] != "working" || o["inputRequests"] != nil || !strings.Contains(o["statusMessage"].(string), "(answer with the reply tool, taskId "+asking.String()+")") ||
		!reflect.DeepEqual(o["_meta"], map[string]any{MetaInputRequired: true}) {
		t.Errorf("a question to a reader without form elicitation: %v", o)
	}

	done := e.task(e.buyer.BuyerID, store.StateCompleted, taskWith(a2a.TaskStateCompleted,
		&a2a.Message{ID: "m", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{a2a.NewTextPart("booked"), a2a.NewDataPart(map[string]any{"day": "friday"})}}, nil), 2, true)
	o = get(form, done)
	res, _ := o["result"].(map[string]any)
	if o["status"] != "completed" || res == nil || res["resultType"] != "complete" || res["isError"] != nil ||
		!reflect.DeepEqual(res["structuredContent"], map[string]any{"taskId": done.String(), "state": "TASK_STATE_COMPLETED",
			"data": []any{map[string]any{"day": "friday"}}}) {
		t.Errorf("a completed task: %v", o)
	}

	failed := e.task(e.buyer.BuyerID, store.StateFailed, taskWith(a2a.TaskStateFailed, nil, map[string]any{store.MetaFailure: "timeout"}), 2, true)
	o = get(form, failed)
	res, _ = o["result"].(map[string]any)
	if o["status"] != "completed" || o["error"] != nil || res == nil || res["isError"] != true ||
		!reflect.DeepEqual(o["_meta"], map[string]any{MetaFailure: "timeout"}) || !reflect.DeepEqual(res["_meta"], map[string]any{MetaFailure: "timeout"}) {
		t.Errorf("a failed task: %v", o)
	}

	canceled := e.task(e.buyer.BuyerID, store.StateCanceled, taskWith(a2a.TaskStateCanceled, nil, map[string]any{store.MetaCanceled: "declined"}), 2, true)
	if o := get(form, canceled); o["status"] != "cancelled" || o["result"] != nil || !reflect.DeepEqual(o["_meta"], map[string]any{MetaCanceled: "declined"}) {
		t.Errorf("a canceled task: %v", o)
	}

	other := Reader{Principal: keyPrincipal(e.team, "default", "kk_other"), Era: mcpwire.EraModern, FormElicitation: true}
	if _, rpcErr, err := e.tasks.Get(context.Background(), other, asking); err != nil || rpcErr == nil || rpcErr.Code != -32602 {
		t.Errorf("another buyer's tasks/get: %v %v", rpcErr, err)
	}
}

// A modern tasks/cancel (§12.5, §11.12's table): acknowledged at once, resultType complete, kontra.cancel in MCP mode
// sent detached on a task that is not terminal (a quoted one included: a cancel declines nothing it was not asked
// to), nothing on a terminal one; another buyer's is -32602.
func TestModernCancel(t *testing.T) {
	e := newModernEnv(t)
	e.cancel.block = make(chan struct{})
	rd := Reader{Principal: e.buyer, Era: mcpwire.EraModern}
	working := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 1, true)
	quoted := e.task(e.buyer.BuyerID, store.StateQuoted, taskWith(a2a.TaskStateInputRequired, nil, map[string]any{lifecycle.MetaApproveURL: "https://x.example/q"}), 1, true)
	done := e.task(e.buyer.BuyerID, store.StateCompleted, taskWith(a2a.TaskStateCompleted, agentText("ok"), nil), 1, true)
	for _, id := range []uuid.UUID{working, quoted, done} {
		began := time.Now()
		res, rpcErr, err := e.tasks.Cancel(context.Background(), rd, id)
		// The lifecycle's cancel blocks until the end of this loop (AcceptWait at most): the answer does not wait for it.
		if err != nil || rpcErr != nil || string(res) != `{"resultType":"complete"}` || time.Since(began) > 5*time.Second {
			t.Errorf("tasks/cancel of %s after %v: %s %v %v", e.state(id), time.Since(began), res, rpcErr, err)
		}
	}
	close(e.cancel.block)
	deadline := time.Now().Add(5 * time.Second)
	for len(e.cancel.calls()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	got := e.cancel.calls()
	if len(got) != 2 || !(got[0] == working.String() && got[1] == quoted.String() || got[1] == working.String() && got[0] == quoted.String()) {
		t.Errorf("kontra.cancel sent for %v; want the working and the quoted task, not the completed one", got)
	}
	other := Reader{Principal: keyPrincipal(e.team, "default", "kk_other"), Era: mcpwire.EraModern}
	if _, rpcErr, err := e.tasks.Cancel(context.Background(), other, working); err != nil || rpcErr == nil || rpcErr.Code != -32602 {
		t.Errorf("another buyer's tasks/cancel: %v %v", rpcErr, err)
	}
}
