package gatemcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/push"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
)

// The tools that follow a hire task (relay.md §12.2; D332..D337), against Postgres.

// toolsHandler is a /mcp handler over e's hire tasks and log, its registry served (nil: every service served).
func toolsHandler(e *modernEnv, served *servedMap) *Handler {
	deps := Deps{PublicURL: "https://gate.example", Hirer: e.hirer, Tasks: e.tasks, Log: store.NewLog(e.pool),
		Waker: noWake{}, PlainWait: 10 * time.Second,
		Catalog: NewCatalog(CatalogConfig{DB: e.pool, PublicURL: "https://gate.example", Logger: slog.New(slog.DiscardHandler)})}
	if served != nil {
		deps.Served = served.served
		e.tasks.served = served.served
	}
	h := &Handler{deps: deps, log: slog.New(slog.DiscardHandler)}
	e.tasks.ref = h.serviceRef
	return h
}

// call is a tools/call of tool with args by p through h, legacy or modern.
func call(t *testing.T, h *Handler, p auth.Principal, modern bool, tool string, args map[string]any) (rendered, answer) {
	t.Helper()
	var (
		rq *request
		w  *httptest.ResponseRecorder
	)
	if modern {
		rq, w = modernRequest(t, "tools/call", map[string]any{"name": tool, "arguments": args}, nil)
		rq.p, rq.modern, rq.start = p, true, time.Now()
	} else {
		rq, w = legacyCall(t, p, tool, args)
	}
	h.toolsCall(rq)
	return toolAnswer(t, w)
}

// event appends one row of kind to task id's log with ev as its StreamResponse, moving the row's version (and, for a
// status event, its state).
func (e *modernEnv) event(id uuid.UUID, kind store.EventKind, ev a2a.Event) int64 {
	e.t.Helper()
	payload := []byte(`{"metadata":{"k":"v"}}`)
	if ev != nil {
		var err error
		if payload, err = json.Marshal(a2a.StreamResponse{Event: ev}); err != nil {
			e.t.Fatal(err)
		}
	}
	var seq int64
	if err := e.pool.QueryRow(context.Background(), `with e as (insert into kontra.task_events (task_id, seq, kind, payload)
			select id, version + 1, $2, $3::jsonb from kontra.tasks where id = $1 returning seq)
		update kontra.tasks set version = (select seq from e) where id = $1 returning version`, id, string(kind), string(payload)).Scan(&seq); err != nil {
		e.t.Fatal(err)
	}
	return seq
}

func structured[T any](t *testing.T, r rendered) T {
	t.Helper()
	b, _ := json.Marshal(r.StructuredContent)
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("structuredContent %s: %v", b, err)
	}
	return v
}

func textOf(r rendered) string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// status (D332), in both eras: the hire result with contextId and service; after:0 the history (task, status, artifact
// and message rows, a metadata row skipped, kontra-blob refs aside), the cursor the last seq read; after:<cursor> nothing
// new and the same cursor; without after the cursor is the tail. Another buyer's task, an unhired one, an unknown id and
// one of a service no longer served are all "unknown task".
func TestStatusTool(t *testing.T) {
	e := newModernEnv(t)
	served := &servedMap{m: map[string]ServedService{"hello": {Kind: config.KindA2A, Allow: allowOf(t, "key:*")}}}
	h := toolsHandler(e, served)
	task := taskWith(a2a.TaskStateCompleted, agentText("booked"), nil, &a2a.Artifact{ID: "a1", Name: "receipt",
		Parts: a2a.ContentParts{a2a.NewTextPart("receipt text"), a2a.NewDataPart(map[string]any{"n": 1})}})
	id := e.task(e.buyer.BuyerID, store.StateCompleted, task, 0, true)
	ctxID := "ctx"
	e.event(id, store.KindStatus, &a2a.TaskStatusUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: ctxID,
		Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: agentText("looking")}})
	e.event(id, store.KindMetadata, nil)
	e.event(id, store.KindArtifact, &a2a.TaskArtifactUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: ctxID,
		Artifact: task.Artifacts[0]})
	e.event(id, store.KindMessage, &a2a.Message{ID: "u2", Role: a2a.MessageRoleUser, Parts: a2a.ContentParts{a2a.NewTextPart("thanks")}})
	last := e.event(id, store.KindStatus, &a2a.TaskStatusUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: ctxID,
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Message: agentText("booked")}})
	for _, modern := range []bool{false, true} {
		r, a := call(t, h, e.buyer, modern, ToolStatus, map[string]any{"taskId": id.String(), "after": 0})
		sr := structured[StatusResult](t, r)
		if a.Error != nil || r.IsError || sr.State != "TASK_STATE_COMPLETED" || sr.ContextID != "ctx" || sr.Service != "hello" ||
			sr.Text != "booked\nreceipt text" || sr.Cursor != last || sr.More {
			t.Fatalf("modern %v: status after 0: %s", modern, a.body)
		}
		var kinds []string
		for _, ev := range sr.Events {
			kinds = append(kinds, ev.Kind+":"+ev.State+":"+ev.Role+":"+ev.Text+":"+ev.ArtifactID)
		}
		want := "status:TASK_STATE_WORKING:agent:looking: artifact:::receipt text:a1 message::user:thanks: " +
			"status:TASK_STATE_COMPLETED:agent:booked:"
		if strings.Join(kinds, " ") != want {
			t.Errorf("modern %v: events %v, want %s", modern, kinds, want)
		}
		if len(sr.Events) > 1 && fmt.Sprint(sr.Events[1].Data) != "[map[n:1]]" {
			t.Errorf("the artifact event's data %v", sr.Events[1].Data)
		}
		if !strings.Contains(textOf(r), `"cursor":`+fmt.Sprint(last)) {
			t.Errorf("the content does not carry the cursor: %s", textOf(r))
		}
		r, _ = call(t, h, e.buyer, modern, ToolStatus, map[string]any{"taskId": id.String(), "after": last})
		if sr := structured[StatusResult](t, r); len(sr.Events) != 0 || sr.Cursor != last {
			t.Errorf("status after the cursor: %+v", sr)
		}
		r, _ = call(t, h, e.buyer, modern, ToolStatus, map[string]any{"taskId": id.String()})
		if sr := structured[StatusResult](t, r); sr.Events != nil || sr.Cursor != last {
			t.Errorf("status without after: %+v", sr)
		}
	}

	other := keyPrincipal(e.team, "default", "kk_other")
	unhired := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, false)
	for what, c := range map[string]struct {
		p  auth.Principal
		id string
	}{
		"another buyer's task": {other, id.String()},
		"an unhired task":      {e.buyer, unhired.String()},
		"an unknown id":        {e.buyer, uuid.Must(uuid.NewV7()).String()},
	} {
		r, a := call(t, h, c.p, false, ToolStatus, map[string]any{"taskId": c.id})
		if !r.IsError || textOf(r) != UnknownTask {
			t.Errorf("%s: %s", what, a.body)
		}
	}
	served.set("hello", nil)
	if r, a := call(t, h, e.buyer, true, ToolStatus, map[string]any{"taskId": id.String()}); !r.IsError || textOf(r) != UnknownTask {
		t.Errorf("a service no longer served: %s", a.body)
	}
	served.set("hello", &ServedService{Kind: config.KindA2A, Allow: allowOf(t, "key:*")})
	for _, bad := range []map[string]any{{"taskId": id.String(), "after": -1}, {"taskId": id.String(), "wait": 26},
		{"taskId": id.String(), "after": 1.5}, {"taskId": "x"}, {"taskId": id.String(), "other": 1}} {
		if r, a := call(t, h, e.buyer, false, ToolStatus, bad); !r.IsError || !strings.HasPrefix(textOf(r), "invalid arguments") {
			t.Errorf("%v: %s", bad, a.body)
		}
	}
}

// status's bounds (D332): at most StatusEvents rows a call, more true when cut, the next call from the cursor.
func TestStatusBounds(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	id := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	for i := range StatusEvents + 10 {
		e.event(id, store.KindStatus, &a2a.TaskStatusUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: "ctx",
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: agentText(fmt.Sprintf("step %d", i))}})
	}
	r, _ := call(t, h, e.buyer, false, ToolStatus, map[string]any{"taskId": id.String(), "after": 0})
	sr := structured[StatusResult](t, r)
	if len(sr.Events) != StatusEvents-1 || !sr.More || sr.Cursor != StatusEvents {
		t.Fatalf("the first page: %d events, more %v, cursor %d", len(sr.Events), sr.More, sr.Cursor)
	}
	r, _ = call(t, h, e.buyer, false, ToolStatus, map[string]any{"taskId": id.String(), "after": sr.Cursor})
	sr = structured[StatusResult](t, r)
	if len(sr.Events) != 11 || sr.More || sr.Events[10].Text != fmt.Sprintf("step %d", StatusEvents+9) {
		t.Fatalf("the second page: %d events, more %v", len(sr.Events), sr.More)
	}
}

// status with wait (D332): nothing after the cursor, it waits until a row lands and answers it; a call whose task has
// ended does not wait; with a progressToken and an event stream accepted, the rows that land are
// notifications/progress before the result (D339).
func TestStatusWaitsAndStreamsProgress(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	id := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	go func() {
		time.Sleep(300 * time.Millisecond)
		e.event(id, store.KindStatus, &a2a.TaskStatusUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: "ctx",
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: agentText("halfway")}})
	}()
	start := time.Now()
	r, a := call(t, h, e.buyer, false, ToolStatus, map[string]any{"taskId": id.String(), "after": 1, "wait": 10})
	sr := structured[StatusResult](t, r)
	if took := time.Since(start); took > 5*time.Second || len(sr.Events) != 1 || sr.Events[0].Text != "halfway" || sr.Cursor != 2 {
		t.Fatalf("after %v: %s", took, a.body)
	}

	// A wait that nothing ends answers at its end, with nothing new.
	start = time.Now()
	r, _ = call(t, h, e.buyer, true, ToolStatus, map[string]any{"taskId": id.String(), "after": 2, "wait": 1})
	if sr := structured[StatusResult](t, r); time.Since(start) < 900*time.Millisecond || len(sr.Events) != 0 || sr.Cursor != 2 {
		t.Fatalf("an empty wait: %+v after %v", sr, time.Since(start))
	}

	// Progress: the request's _meta.progressToken and Accept: text/event-stream.
	go func() {
		time.Sleep(300 * time.Millisecond)
		e.event(id, store.KindArtifact, &a2a.TaskArtifactUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: "ctx",
			Artifact: &a2a.Artifact{ID: "a1", Name: "draft", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}})
	}()
	rq, w := modernRequest(t, "tools/call", map[string]any{"name": ToolStatus,
		"arguments": map[string]any{"taskId": id.String(), "after": 2, "wait": 10},
		"_meta": map[string]any{"progressToken": "tok-1", "io.modelcontextprotocol/protocolVersion": "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{}}}, nil)
	rq.r.Header.Set("Accept", "application/json, text/event-stream")
	rq.p, rq.modern, rq.start = e.buyer, true, time.Now()
	h.toolsCall(rq)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q: %s", ct, w.Body)
	}
	var msgs []map[string]any
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id:") {
			t.Errorf("an event id on /mcp's response stream: %q", line)
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(d), &m); err != nil {
				t.Fatal(err)
			}
			msgs = append(msgs, m)
		}
	}
	if len(msgs) != 2 || msgs[0]["method"] != "notifications/progress" || msgs[1]["result"] == nil {
		t.Fatalf("the stream: %s", w.Body)
	}
	params := msgs[0]["params"].(map[string]any)
	if params["progressToken"] != "tok-1" || params["progress"] != 1.0 || params["message"] != "artifact draft" {
		t.Errorf("the progress notification: %v", params)
	}
}

// tasks (D333): the buyer's hire tasks newest first, 50 a page with an opaque cursor; filtered by service, contextId
// and state (an A2A state, active, ended); never another buyer's, an unhired task or one whose service is not served.
func TestTasksTool(t *testing.T) {
	e := newModernEnv(t)
	served := &servedMap{m: map[string]ServedService{"hello": {Kind: config.KindA2A, Allow: allowOf(t, "key:*")}}}
	h := toolsHandler(e, served)
	var ids []uuid.UUID
	for i := range 55 {
		st, ts := store.StateWorking, a2a.TaskStateWorking
		if i%5 == 0 {
			st, ts = store.StateCompleted, a2a.TaskStateCompleted
		}
		id := e.task(e.buyer.BuyerID, st, taskWith(ts, agentText(strings.Repeat("y", 300)), nil), 0, true)
		ids = append(ids, id)
		if _, err := e.pool.Exec(context.Background(), `update kontra.tasks set status_at = now() + $2 * interval '1 second' where id = $1`, id, i); err != nil {
			t.Fatal(err)
		}
	}
	e.task(keyPrincipal(e.team, "default", "kk_other").BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, false)
	r, a := call(t, h, e.buyer, false, ToolTasks, map[string]any{})
	page := structured[TasksResult](t, r)
	if a.Error != nil || len(page.Tasks) != 50 || page.NextCursor == "" || page.Tasks[0].TaskID != ids[54].String() ||
		page.Tasks[0].Service != "hello" || page.Tasks[0].ContextID != "ctx" || len([]rune(page.Tasks[0].Text)) != TaskTextMax {
		t.Fatalf("the first page: %s", a.body)
	}
	r, _ = call(t, h, e.buyer, true, ToolTasks, map[string]any{"cursor": page.NextCursor})
	if p2 := structured[TasksResult](t, r); len(p2.Tasks) != 5 || p2.NextCursor != "" || p2.Tasks[4].TaskID != ids[0].String() {
		t.Fatalf("the second page: %+v", p2)
	}
	for state, n := range map[string]int{"ended": 11, "active": 44, "TASK_STATE_COMPLETED": 11, "TASK_STATE_WORKING": 44, "TASK_STATE_FAILED": 0} {
		r, _ := call(t, h, e.buyer, false, ToolTasks, map[string]any{"state": state})
		if got := structured[TasksResult](t, r); len(got.Tasks) != min(n, 50) {
			t.Errorf("state %s: %d tasks, want %d", state, len(got.Tasks), n)
		}
	}
	for args, n := range map[string]int{`{"service":"hello","contextId":"ctx","state":"ended"}`: 11, `{"service":"nope"}`: 0,
		`{"contextId":"other"}`: 0} {
		var m map[string]any
		_ = json.Unmarshal([]byte(args), &m)
		r, _ := call(t, h, e.buyer, false, ToolTasks, m)
		if got := structured[TasksResult](t, r); len(got.Tasks) != n {
			t.Errorf("%s: %d tasks, want %d", args, len(got.Tasks), n)
		}
	}
	if r, a := call(t, h, e.buyer, false, ToolTasks, map[string]any{"cursor": "forged"}); !r.IsError {
		t.Errorf("a forged cursor: %s", a.body)
	}
	if r, a := call(t, h, e.buyer, false, ToolTasks, map[string]any{"state": "working"}); !r.IsError {
		t.Errorf("a state that is none: %s", a.body)
	}
	served.set("hello", nil)
	if r, _ := call(t, h, e.buyer, false, ToolTasks, map[string]any{}); len(structured[TasksResult](t, r).Tasks) != 0 {
		t.Errorf("tasks of a service no longer served are listed")
	}
}

// The tasks tool reads at most HiredReadBound rows a request (D333, as search, D293): a request that reaches the bound
// answers what matched, here nothing, with the cursor at the last row it read, and the next request goes on from it.
func TestTasksBoundsTheRowsOneRequestReads(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id,
			state, version, snapshot, status_at, submitted_at, hired)
		select gen_random_uuid(), $1, 'hello', 'a2a', $2, 'key:kk_buyer', 'api_key', 'ctx', 'working', 1, '{"id":"x"}',
			now() - i * interval '1 second', now(), true
		from generate_series(1, $3::int) i`, e.team, e.buyer.BuyerID, store.HiredReadBound+5); err != nil {
		t.Fatal(err)
	}
	old := e.task(e.buyer.BuyerID, store.StateCompleted, taskWith(a2a.TaskStateCompleted, agentText("old"), nil), 0, true)
	if _, err := e.pool.Exec(ctx, `update kontra.tasks set status_at = now() - interval '1 day' where id = $1`, old); err != nil {
		t.Fatal(err)
	}
	r, _ := call(t, h, e.buyer, false, ToolTasks, map[string]any{"state": "ended"})
	first := structured[TasksResult](t, r)
	if len(first.Tasks) != 0 || first.NextCursor == "" {
		t.Fatalf("the bounded read: %+v", first)
	}
	r, _ = call(t, h, e.buyer, false, ToolTasks, map[string]any{"state": "ended", "cursor": first.NextCursor})
	if next := structured[TasksResult](t, r); len(next.Tasks) != 1 || next.Tasks[0].TaskID != old.String() || next.NextCursor != "" {
		t.Fatalf("the next read: %+v", next)
	}
}

// cancel (D334): MCP tasks/cancel's legacy semantics, awaited, answering the canceled task's hire result without
// isError; an ended task is isError "the task already ended (<state>)" and sends nothing; another buyer's is unknown.
func TestCancelTool(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	id := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	e.cancel = &cancelRecorder{}
	e.tasks.lc = cancelling{e: e}
	r, a := call(t, h, e.buyer, false, ToolCancel, map[string]any{"taskId": id.String()})
	hr := structured[HireResult](t, r)
	if a.Error != nil || r.IsError || hr.State != "TASK_STATE_CANCELED" || hr.Service != "hello" || e.state(id) != "canceled" {
		t.Fatalf("cancel: %s", a.body)
	}
	r, a = call(t, h, e.buyer, true, ToolCancel, map[string]any{"taskId": id.String()})
	if !r.IsError || textOf(r) != "the task already ended (canceled)" {
		t.Fatalf("cancel of an ended task: %s", a.body)
	}
	other := keyPrincipal(e.team, "default", "kk_other")
	running := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	if r, _ := call(t, h, other, false, ToolCancel, map[string]any{"taskId": running.String()}); !r.IsError || textOf(r) != UnknownTask ||
		e.state(running) != "working" {
		t.Errorf("another buyer's cancel")
	}
}

// cancelling is the lifecycle's awaited MCP-mode cancel as the workflow answers it: the task canceled.
type cancelling struct{ e *modernEnv }

func (c cancelling) CancelMCP(ctx context.Context, id a2a.TaskID) (lifecycle.CancelResult, bool, error) {
	task := taskWith(a2a.TaskStateCanceled, nil, map[string]any{store.MetaCanceled: store.CanceledClient})
	task.ID = id
	snap, _ := json.Marshal(task)
	_, err := c.e.pool.Exec(ctx, `with e as (insert into kontra.task_events (task_id, seq, kind, payload)
			select id, version + 1, 'status', '{}' from kontra.tasks where id = $1 returning seq)
		update kontra.tasks set state = 'canceled', version = (select seq from e), snapshot = $2::json, terminal_at = now()
		where id = $1`, uuid.MustParse(string(id)), string(snap))
	return lifecycle.CancelResult{}, true, err
}

func (c cancelling) RequestCancelMCP(ctx context.Context, id a2a.TaskID) error {
	_, _, err := c.CancelMCP(ctx, id)
	return err
}

// artifact (D335): one artifact in full (text, data as JSON and in data, a raw part embedded, a URL part linked);
// an artifact id the task does not hold is "unknown artifact".
func TestArtifactTool(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	raw := a2a.NewRawPart([]byte("tck"))
	raw.Filename, raw.MediaType = "out.txt", "text/plain"
	id := e.task(e.buyer.BuyerID, store.StateCompleted, taskWith(a2a.TaskStateCompleted, agentText("done"), nil,
		&a2a.Artifact{ID: "a1", Name: "report", Description: "the report", Parts: a2a.ContentParts{a2a.NewTextPart("body"),
			a2a.NewDataPart(map[string]any{"k": "v"}), raw, a2a.NewFileURLPart("https://files.example/r.pdf", "application/pdf")}}), 0, true)
	r, a := call(t, h, e.buyer, true, ToolArtifact, map[string]any{"taskId": id.String(), "artifactId": "a1"})
	ar := structured[ArtifactResult](t, r)
	if a.Error != nil || r.IsError || ar.Name != "report" || ar.Description != "the report" || ar.Text != "body" ||
		fmt.Sprint(ar.Data) != "[map[k:v]]" || len(ar.Files) != 2 || ar.Files[1].URI != "https://files.example/r.pdf" ||
		len(r.Content) != 4 || r.Content[2].Type != "resource" || r.Content[3].Type != "resource_link" {
		t.Fatalf("the artifact: %s", a.body)
	}
	if r, a := call(t, h, e.buyer, false, ToolArtifact, map[string]any{"taskId": id.String(), "artifactId": "nope"}); !r.IsError ||
		textOf(r) != "unknown artifact" {
		t.Errorf("an unknown artifact: %s", a.body)
	}
}

// nopSignal is the lifecycle's kontra.push, sent nowhere.
type nopSignal struct{}

func (nopSignal) SignalPush(context.Context, a2a.TaskID, lifecycle.PushSignal) error { return nil }

// push_set, push_list and push_delete (D337) on the A2A push config store (§10.1): a URL §10.3 refuses (loopback) is
// isError and stores nothing; a public https URL is stored once (an idempotent save), its token and credentials never
// shown; delete removes it. Another buyer's task is unknown.
func TestPushTools(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	h.deps.Push = push.NewConfigStore(e.pool, nopSignal{}, ssrf.Policy{Resolver: fixedResolver{}}, slog.New(slog.DiscardHandler))
	id := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	r, a := call(t, h, e.buyer, false, ToolPushSet, map[string]any{"taskId": id.String(), "url": "https://127.0.0.1/hook"})
	if !r.IsError || !strings.Contains(textOf(r), "nothing was stored") {
		t.Fatalf("a loopback URL: %s", a.body)
	}
	args := map[string]any{"taskId": id.String(), "url": "https://hooks.example/k", "token": "secret-token",
		"authentication": map[string]any{"scheme": "Bearer", "credentials": "secret-creds"}}
	for range 2 {
		r, a = call(t, h, e.buyer, true, ToolPushSet, args)
		if r.IsError || strings.Contains(a.body, "secret") {
			t.Fatalf("push_set: %s", a.body)
		}
	}
	set := structured[PushResult](t, r)
	r, a = call(t, h, e.buyer, false, ToolPushList, map[string]any{"taskId": id.String()})
	list := structured[PushResult](t, r)
	if len(list.Configs) != 1 || list.Configs[0] != set.Configs[0] || !list.Configs[0].Token || list.Configs[0].Scheme != "Bearer" {
		t.Fatalf("push_list: %s", a.body)
	}
	other := keyPrincipal(e.team, "default", "kk_other")
	if r, _ := call(t, h, other, false, ToolPushList, map[string]any{"taskId": id.String()}); !r.IsError || textOf(r) != UnknownTask {
		t.Errorf("another buyer's push_list")
	}
	r, _ = call(t, h, e.buyer, false, ToolPushDelete, map[string]any{"taskId": id.String(), "configId": set.Configs[0].ConfigID})
	if got := structured[PushResult](t, r); r.IsError || len(got.Configs) != 0 {
		t.Fatalf("push_delete: %+v", got)
	}
}

// fixedResolver resolves every host to a public documentation-free address, so that §10.3's check passes for a
// hostname and refuses a loopback literal.
type fixedResolver struct{}

func (fixedResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
}

// reply's refusals say what to do (D336): a running task names status, an ended one hire with its contextId.
func TestReplyRefusalsSayWhatToDo(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	running := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	ended := e.task(e.buyer.BuyerID, store.StateCompleted, taskWith(a2a.TaskStateCompleted, agentText("done"), nil), 0, true)
	r, _ := call(t, h, e.buyer, false, ToolReply, map[string]any{"taskId": running.String(), "message": "more"})
	if !r.IsError || !strings.Contains(textOf(r), `follow it with status {taskId: "`+running.String()+`", after: 0, wait: 25}`) {
		t.Errorf("a running task: %s", textOf(r))
	}
	r, _ = call(t, h, e.buyer, true, ToolReply, map[string]any{"taskId": ended.String(), "message": "more"})
	if !r.IsError || !strings.Contains(textOf(r), `Continue the conversation with hire {service: "hello", contextId: "ctx", message}`) {
		t.Errorf("an ended task: %s", textOf(r))
	}
	if len(e.hirer.replies()) != 0 {
		t.Errorf("a refused reply was sent")
	}
}

// hire with an idempotencyKey (D338) as the handler answers it: the key's messageId is the same for the principal's
// every hire of it and differs for another key or principal; a key the Hirer finds used for other arguments is isError
// and creates nothing; a malformed key breaks the input schema.
func TestHireIdempotencyKeyArguments(t *testing.T) {
	team := uuid.Must(uuid.NewV7())
	p := keyPrincipal(team, "default", "kk_a")
	q := keyPrincipal(team, "default", "kk_b")
	if KeyedMessageID(p, "k1") != KeyedMessageID(p, "k1") || KeyedMessageID(p, "k1") == KeyedMessageID(p, "k2") ||
		KeyedMessageID(p, "k1") == KeyedMessageID(q, "k1") {
		t.Fatal("KeyedMessageID")
	}
	for _, bad := range []string{"", "has space", strings.Repeat("x", 129), "é"} {
		if _, _, err := parseHireArgs(json.RawMessage(`{"service":"hello","message":"hi","idempotencyKey":` + fmt.Sprintf("%q", bad) + `}`)); err == nil {
			t.Errorf("idempotencyKey %q accepted", bad)
		}
	}
	if a, _, err := parseHireArgs(json.RawMessage(`{"service":"hello","message":"hi","idempotencyKey":"run-7:a.b_c"}`)); err != nil ||
		a.IdempotencyKey != "run-7:a.b_c" {
		t.Errorf("a good key: %+v %v", a, err)
	}
	if !errors.Is(lifecycle.ErrHireKeyReused, a2a.ErrInvalidParams) {
		t.Error("ErrHireKeyReused is not invalid params")
	}
}
