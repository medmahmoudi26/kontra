package gatemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/relay"
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

// cancelRecorder is the lifecycle's MCP-mode cancel, awaited (CancelMCP) or accepted only (RequestCancelMCP),
// recorded; block, when set, holds every call until closed; err, when set, is RequestCancelMCP's answer.
type cancelRecorder struct {
	mu    sync.Mutex
	ids   []string
	block chan struct{}
	err   error
}

func (c *cancelRecorder) CancelMCP(ctx context.Context, id a2a.TaskID) (lifecycle.CancelResult, bool, error) {
	return lifecycle.CancelResult{}, true, c.RequestCancelMCP(ctx, id)
}

func (c *cancelRecorder) RequestCancelMCP(ctx context.Context, id a2a.TaskID) error {
	c.mu.Lock()
	block := c.block
	c.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, string(id))
	return c.err
}

func (c *cancelRecorder) set(block chan struct{}, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.block, c.err = block, err
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

// modernCaps is a modern request's _meta: the tasks extension, and form elicitation when form.
func modernCaps(form bool) map[string]any {
	caps := map[string]any{"extensions": map[string]any{mcpwire.ExtTasks: map[string]any{}}}
	if form {
		caps["elicitation"] = map[string]any{"form": map[string]any{}}
	}
	return map[string]any{mcpwire.MetaProtocolVersion: mcpwire.V20260728, mcpwire.MetaClientCapabilities: caps}
}

// served is a modern request's answer through the handler (status, JSON-RPC error or result, Retry-After).
type served struct {
	answer
	retryAfter string
}

// serve answers a modern method of p with params (its _meta added: the tasks extension, form elicitation when form)
// through h, as /mcp writes it.
func serve(t *testing.T, h *Handler, p auth.Principal, method string, params map[string]any, form bool) served {
	t.Helper()
	params["_meta"] = modernCaps(form)
	rq, w := modernRequest(t, method, params, nil)
	rq.p = p
	h.modern(rq)
	return served{answer: answerOf(t, w), retryAfter: w.Header().Get("Retry-After")}
}

// A modern tasks/update (§12.4, §12.5, D274; the corrections' WP-05 rule): the one answerable key is the gate id of
// the question outstanding now, as the reader's own request sees it in tasks/get; its accept {reply} is the §7
// follow-up, once. A quoted task (D38, D45), a task asking nothing, an earlier turn's key, another task's key, a
// malformed key, a decline, an accept without a string reply, a question the reader did not declare form elicitation
// for, and a question that is not text only are all acknowledged and forward nothing. Another buyer's task is
// -32602, as an unknown one; inputResponses that is not an object is -32602. The outstanding key's answer that the A2A
// route refuses is acknowledged only when the question stopped being outstanding; any other refusal is an error the
// client can answer again after (503 for the lifecycle or the gate, 429 for a limit, the A2A refusal's JSON-RPC error).
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

	// The outstanding key's answer, refused by the A2A route (M4 WP-05 review): never the acknowledgement, which tells
	// the client its answer was taken (the extension: a key is answered once), so the buyer can answer again. A lifecycle
	// that did not accept it in time and the gate's own failure 503 with -32603; an anonymous tenant's limit 429 with
	// Retry-After; another A2A refusal its JSON-RPC error. Each leaves the task waiting, and nothing was taken.
	h := &Handler{deps: Deps{Tasks: e.tasks}, log: slog.New(slog.DiscardHandler)}
	answering := map[string]any{"taskId": asking.String(), "inputResponses": map[string]any{key(asking, 3): map[string]any{
		"action": "accept", "content": map[string]any{"reply": "friday"}}}}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   int
		msg    string
	}{
		{"a lifecycle that did not accept it in time", fmt.Errorf("not accepted: %w", lifecycle.ErrUnavailable), 503, -32603, ""},
		{"the gate's own failure", errors.New("pgx: connection refused"), 503, -32603, "the gate could not serve the request"},
		{"an A2A internal error", a2a.NewError(a2a.ErrInternalError, "the request produced no task"), 200, -32603, "the request produced no task"},
		{"the anonymous storage limit", a2a.NewError(a2a.ErrInvalidParams, "anonymous storage limit"), 200, -32602, "anonymous storage limit"},
		{"a required extension", &relay.ExtensionsRequiredError{URIs: []string{"https://kontra.sh/ext/escrow/v1"}}, 200, -32008,
			"https://kontra.sh/ext/escrow/v1"},
		{"an anonymous tenant's limit", lifecycle.RateLimited(), 429, -32600, "rate limited"},
	} {
		e.hirer.err = tc.err
		a := serve(t, h, e.buyer, "tasks/update", maps.Clone(answering), true)
		if a.status != tc.status || a.Error == nil || a.Error.Code != tc.code || !strings.Contains(a.Error.Message, tc.msg) {
			t.Errorf("%s: %d %s, want %d with %d", tc.name, a.status, a.body, tc.status, tc.code)
		}
		if tc.status == 429 && a.retryAfter != "5" {
			t.Errorf("%s: Retry-After %q", tc.name, a.retryAfter)
		}
		if e.state(asking) != "input-required" {
			t.Fatalf("%s: the task is %s", tc.name, e.state(asking))
		}
	}
	// Refused because the question stopped being outstanding (another follow-up of the turn was taken first, §7):
	// acknowledged, as an answer to a key that is not.
	e.hirer.err = a2a.NewError(a2a.ErrUnsupportedOperation, "a follow-up of this turn was accepted first")
	if a := serve(t, h, e.buyer, "tasks/update", maps.Clone(answering), true); a.status != 200 || a.Error != nil ||
		string(a.Result) != `{"resultType":"complete"}` {
		t.Errorf("a refused follow-up: %d %s", a.status, a.body)
	}
	e.hirer.err = nil
	if got := e.hirer.replies(); len(got) != 0 {
		t.Fatalf("refused answers were taken: %v", got)
	}

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

// A modern tasks/get of a hire task (§12.5, §11.12's table, D274): resultType complete, ttlMs and pollIntervalMs (not
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
	block := make(chan struct{})
	e.cancel.set(block, nil)
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
	close(block)
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

// The pairing of an inputRequests key with its question (M4 WP-05 review): tasks/get reads the task and the seq of the
// event that asked in one statement, so a task that answers and asks again meanwhile never shows one question's text
// under another question's key (the tasks extension: a key MUST NOT name two distinct requests). A writer cycles the
// task through working and a new question "Q <seq>" while a reader polls; every input_required answer must carry
// exactly the key k.<task>.<n> of its text's n.
func TestModernGetPairsEachKeyWithItsQuestion(t *testing.T) {
	e := newModernEnv(t)
	form := Reader{Principal: e.buyer, Era: mcpwire.EraModern, FormElicitation: true}
	id := e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Q 3"), nil), 2, true)
	ctx := context.Background()
	step := func(state store.State, snap *a2a.Task) {
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var v int64
		if err := tx.QueryRow(ctx, `select version from kontra.tasks where id = $1 for update`, id).Scan(&v); err != nil {
			t.Error(err)
			return
		}
		snap.ID = a2a.TaskID(id.String())
		if state == store.StateInputRequired {
			snap.Status.Message = agentText(fmt.Sprintf("Q %d", v+1))
		}
		b, _ := json.Marshal(snap)
		if _, err := tx.Exec(ctx, `insert into kontra.task_events (task_id, seq, kind, payload) values ($1, $2, 'status', '{}')`, id, v+1); err != nil {
			t.Error(err)
			return
		}
		if _, err := tx.Exec(ctx, `update kontra.tasks set state = $2, version = $3, snapshot = $4::json where id = $1`, id, string(state), v+1, string(b)); err != nil {
			t.Error(err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Error(err)
		}
	}
	var done atomic.Bool
	go func() {
		defer done.Store(true)
		for range 300 {
			step(store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil))
			step(store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, nil, nil))
		}
	}()
	asked, mismatched := 0, 0
	for !done.Load() {
		res, rpcErr, err := e.tasks.Get(ctx, form, id)
		if err != nil || rpcErr != nil {
			t.Fatalf("tasks/get: %v %v", rpcErr, err)
		}
		var o struct {
			Status        string `json:"status"`
			InputRequests map[string]struct {
				Params struct {
					Message string `json:"message"`
				} `json:"params"`
			} `json:"inputRequests"`
		}
		if err := json.Unmarshal(res, &o); err != nil {
			t.Fatal(err)
		}
		if o.Status != "input_required" {
			continue
		}
		asked++
		for k, r := range o.InputRequests {
			var n int64
			if _, err := fmt.Sscanf(r.Params.Message, "Q %d", &n); err != nil || k != store.GateRequestID(id, n) || len(o.InputRequests) != 1 {
				mismatched++
				if mismatched <= 3 {
					t.Errorf("tasks/get showed %q under %s", r.Params.Message, k)
				}
			}
		}
	}
	if mismatched > 0 {
		t.Fatalf("%d of %d input_required answers paired a question with another question's key", mismatched, asked)
	}
	if asked == 0 {
		t.Fatal("the reader never saw the task asking")
	}
	t.Logf("%d input_required answers, all paired", asked)
}

// A modern tasks/cancel's detached cancel is coalesced per task on the replica (M4 WP-05 review): while one is being
// sent, and for the settle time after it was accepted, a tasks/cancel of the task sends nothing more (fifty of them make
// one RequestCancelMCP); one that was not accepted is forgotten, so the next tasks/cancel sends again; after the settle
// time a new one is sent. Wait returns only once no detached cancel and no modern hire (hold) is in flight.
func TestModernCancelCoalesces(t *testing.T) {
	e := newModernEnv(t)
	rd := Reader{Principal: e.buyer, Era: mcpwire.EraModern}
	working := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 1, true)
	other := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 1, true)
	e.tasks.settle = 500 * time.Millisecond
	ctx := context.Background()
	cancel := func(id uuid.UUID) {
		t.Helper()
		if res, rpcErr, err := e.tasks.Cancel(ctx, rd, id); err != nil || rpcErr != nil || string(res) != `{"resultType":"complete"}` {
			t.Fatalf("tasks/cancel: %s %v %v", res, rpcErr, err)
		}
	}
	waitFor := func(d time.Duration) error {
		wctx, stop := context.WithTimeout(ctx, d)
		defer stop()
		return e.tasks.Wait(wctx)
	}
	calls := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(e.cancel.calls()) < n && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		if got := len(e.cancel.calls()); got != n {
			t.Fatalf("RequestCancelMCP called %d times, want %d", got, n)
		}
	}

	if err := waitFor(time.Second); err != nil {
		t.Fatalf("Wait with nothing in flight: %v", err)
	}
	block := make(chan struct{})
	e.cancel.set(block, nil)
	for range 50 {
		cancel(working)
	}
	cancel(other)
	if err := waitFor(100 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait while two cancels are being sent: %v", err)
	}
	close(block)
	if err := waitFor(5 * time.Second); err != nil {
		t.Fatalf("Wait after the cancels were accepted: %v", err)
	}
	calls(2) // one per task
	for range 10 {
		cancel(working) // accepted less than the settle time ago
	}
	calls(2)
	time.Sleep(600 * time.Millisecond)
	cancel(working) // past it
	calls(3)

	// One that was not accepted is forgotten: the next tasks/cancel sends again.
	e.cancel.set(nil, fmt.Errorf("not accepted: %w", lifecycle.ErrUnavailable))
	cancel(other)
	calls(4)
	if err := waitFor(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	e.cancel.set(nil, nil)
	cancel(other)
	calls(5)

	// A modern hire in flight holds Wait as a cancel does.
	release := e.tasks.hold()
	if err := waitFor(100 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait while a modern hire is answered: %v", err)
	}
	release()
	release()
	if err := waitFor(time.Second); err != nil {
		t.Fatalf("Wait after the hire: %v", err)
	}
}

// slowHirer is a Hirer whose Hire takes delay (a slow Admission) and then creates a working hire task of the buyer.
type slowHirer struct {
	*replyRecorder
	e     *modernEnv
	delay time.Duration
}

func (s *slowHirer) Hire(ctx context.Context, req HireRequest) (HireOutcome, error) {
	time.Sleep(s.delay)
	id := s.e.task(req.Principal.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 1, true)
	row, err := store.NewLog(s.e.pool).Load(ctx, id)
	return HireOutcome{Task: row}, err
}

// A plain hire's wait counts from the request's arrival (M4 WP-05 review): after an Admission that took most of the
// budget, a modern plain hire is still answered within PlainBudget (here 2.5 s for a 2 s plain wait after a 1.5 s
// Admission), before its client's request timeout (here 3 s, the MCP SDKs' 60 s scaled), so its task is not cancelled
// as the task of a client that left (D178). The legacy plain hire answers within the same budget.
func TestPlainWaitCountsFromTheRequest(t *testing.T) {
	e := newModernEnv(t)
	hirer := &slowHirer{replyRecorder: e.hirer, e: e, delay: 1500 * time.Millisecond}
	h := &Handler{deps: Deps{PublicURL: "https://gate.example", Catalog: NewCatalog(CatalogConfig{DB: e.pool, Logger: slog.New(slog.DiscardHandler)}),
		Hirer: hirer, Tasks: e.tasks, Log: store.NewLog(e.pool), Waker: noWake{}, PlainWait: 2 * time.Second,
		PlainBudget: 2500 * time.Millisecond}, log: slog.New(slog.DiscardHandler)}
	for _, modern := range []bool{true, false} {
		params := map[string]any{"name": "hire", "arguments": map[string]any{"service": "hello", "message": "hi"}}
		rq, w := modernRequest(t, "tools/call", params, nil)
		rq.p, rq.start, rq.modern = e.buyer, time.Now(), modern
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // the client's request timeout
		rq.r = rq.r.WithContext(ctx)
		h.toolsCall(rq)
		took := time.Since(rq.start)
		cancel()
		a := answerOf(t, w)
		if a.status != 200 || a.Error != nil || !strings.Contains(string(a.Result), "is still working") || took > 2900*time.Millisecond {
			t.Errorf("modern %v: answered after %v: %d %s", modern, took, a.status, a.body)
		}
		time.Sleep(200 * time.Millisecond)
		if got := e.cancel.calls(); len(got) != 0 {
			t.Fatalf("modern %v: the task of a client that was answered in time was cancelled: %v", modern, got)
		}
	}
}
