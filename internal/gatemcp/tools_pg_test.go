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
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pgtest"
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
		// With after, the result is a summary: the events carry the artifacts (D332), so a page repeats none of them.
		if a.Error != nil || r.IsError || sr.State != "TASK_STATE_COMPLETED" || sr.ContextID != "ctx" || sr.Service != "hello" ||
			!strings.HasPrefix(sr.Text, "The task ended completed.\nbooked\nIts artifacts are in the events") || sr.Data != nil ||
			sr.Cursor != last || sr.More || len(r.Content) != 2 || strings.Contains(r.Content[0].Text, "receipt text") {
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
		if sr := structured[StatusResult](t, r); sr.Events != nil || sr.Cursor != last || sr.Text != "booked\nreceipt text" || len(sr.Data) != 1 {
			t.Errorf("status without after (the whole hire result): %+v", sr)
		}
		// An after beyond the tail is read as the tail: the cursor never skips a row that lands later.
		r, _ = call(t, h, e.buyer, modern, ToolStatus, map[string]any{"taskId": id.String(), "after": last + 1000})
		if sr := structured[StatusResult](t, r); len(sr.Events) != 0 || sr.Cursor != last {
			t.Errorf("status after a seq beyond the tail: %+v", sr)
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

// chanWaker is a waker the test fires: every wait that subscribed is woken by wake.
type chanWaker struct {
	mu  sync.Mutex
	chs []chan struct{}
}

func (w *chanWaker) WaitTask(context.Context, uuid.UUID) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.chs = append(w.chs, ch)
	w.mu.Unlock()
	return ch, func() {}
}

func (w *chanWaker) wake() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.chs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// later runs f after d, in its own goroutine.
func later(d time.Duration, f func()) {
	go func() {
		time.Sleep(d)
		f()
	}()
}

// status with wait (D332), its waits woken only by the waker (the reread is an hour): nothing after the cursor, it
// waits until a status row lands and answers it, its running text naming the cursor it returns; the principal's own
// message row (a reply) does not end the wait, the status row after it does; a task at input-required, or ended, does
// not wait; a wait that nothing ends answers at its end; a task whose service stops being served during the wait is
// "unknown task" when the wait ends (D303).
func TestStatusWaits(t *testing.T) {
	e := newModernEnv(t)
	served := &servedMap{m: map[string]ServedService{"hello": {Kind: config.KindA2A, Allow: allowOf(t, "key:*")}}}
	h := toolsHandler(e, served)
	wk := &chanWaker{}
	h.deps.Waker, h.deps.rereadEvery = wk, time.Hour
	id := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	status := func(text string) func() {
		return func() {
			e.event(id, store.KindStatus, &a2a.TaskStatusUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: "ctx",
				Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: agentText(text)}})
			wk.wake()
		}
	}
	for _, modern := range []bool{false, true} {
		cursor := func() int64 {
			var n int64
			if err := e.pool.QueryRow(context.Background(), `select version from kontra.tasks where id = $1`, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}()
		later(300*time.Millisecond, status("halfway"))
		start := time.Now()
		r, a := call(t, h, e.buyer, modern, ToolStatus, map[string]any{"taskId": id.String(), "after": cursor, "wait": 10})
		sr := structured[StatusResult](t, r)
		if took := time.Since(start); took > 5*time.Second || len(sr.Events) != 1 || sr.Events[0].Text != "halfway" || sr.Cursor != cursor+1 {
			t.Fatalf("modern %v, after %v: %s", modern, took, a.body)
		}
		if want := fmt.Sprintf("after: %d, wait: 25}", sr.Cursor); !strings.Contains(sr.Text, want) {
			t.Errorf("modern %v: the running text does not name the cursor (%s): %s", modern, want, sr.Text)
		}

		// The principal's reply lands as a message row first: the wait goes on until the agent's status row.
		cursor = sr.Cursor
		later(300*time.Millisecond, func() {
			e.event(id, store.KindMessage, &a2a.Message{ID: "u" + fmt.Sprint(modern), Role: a2a.MessageRoleUser,
				Parts: a2a.ContentParts{a2a.NewTextPart("sat")}})
			e.event(id, store.KindMetadata, nil)
			wk.wake()
			time.Sleep(400 * time.Millisecond)
			status("on it")()
		})
		start = time.Now()
		r, a = call(t, h, e.buyer, modern, ToolStatus, map[string]any{"taskId": id.String(), "after": cursor, "wait": 10})
		sr = structured[StatusResult](t, r)
		if took := time.Since(start); took < 600*time.Millisecond || len(sr.Events) != 2 || sr.Events[0].Kind != "message" ||
			sr.Events[1].Text != "on it" {
			t.Fatalf("modern %v: a wait across the principal's own message row, after %v: %s", modern, took, a.body)
		}

		// A wait that nothing ends answers at its end, with nothing new.
		start = time.Now()
		r, _ = call(t, h, e.buyer, modern, ToolStatus, map[string]any{"taskId": id.String(), "after": sr.Cursor, "wait": 1})
		if got := structured[StatusResult](t, r); time.Since(start) < 900*time.Millisecond || len(got.Events) != 0 || got.Cursor != sr.Cursor {
			t.Fatalf("modern %v: an empty wait: %+v after %v", modern, got, time.Since(start))
		}
	}

	// A task at input-required moves only when this principal replies: status answers at once.
	asking := e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil), 0, true)
	start := time.Now()
	r, a := call(t, h, e.buyer, true, ToolStatus, map[string]any{"taskId": asking.String(), "after": 1, "wait": 10})
	if took := time.Since(start); took > 2*time.Second || structured[StatusResult](t, r).State != "TASK_STATE_INPUT_REQUIRED" {
		t.Fatalf("an input-required task waited %v: %s", took, a.body)
	}

	// The service stops being served during the wait: the answer is "unknown task", not the row that landed.
	r, _ = call(t, h, e.buyer, false, ToolStatus, map[string]any{"taskId": id.String()})
	tail := structured[StatusResult](t, r).Cursor
	landed := make(chan struct{})
	later(300*time.Millisecond, func() {
		defer close(landed)
		served.set("hello", nil)
		status("secret progress")()
	})
	r, a = call(t, h, e.buyer, false, ToolStatus, map[string]any{"taskId": id.String(), "after": tail, "wait": 10})
	<-landed
	if !r.IsError || textOf(r) != UnknownTask || strings.Contains(a.body, "secret progress") {
		t.Fatalf("a service no longer served after the wait: %s", a.body)
	}
}

// progressStream is the JSON-RPC messages of an event stream answer, failing on an event id (/mcp's response streams
// carry none).
func progressStream(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
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
	return msgs
}

// progressCall is a tools/call of tool with args by p through h, in either era, with _meta.progressToken tok and an
// event stream accepted.
func progressCall(t *testing.T, h *Handler, p auth.Principal, modern bool, tool string, args map[string]any, tok string) *httptest.ResponseRecorder {
	t.Helper()
	rq, w := modernRequest(t, "tools/call", map[string]any{"name": tool, "arguments": args,
		"_meta": map[string]any{"progressToken": tok, "io.modelcontextprotocol/protocolVersion": "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{}}}, nil)
	rq.r.Header.Set("Accept", "application/json, text/event-stream")
	rq.p, rq.modern, rq.start = p, modern, time.Now()
	h.toolsCall(rq)
	return w
}

// status with wait and a progressToken (D339), in both eras: the status and artifact rows after the cursor are
// notifications/progress, then the result, on one event stream without event ids.
func TestStatusStreamsProgress(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	wk := &chanWaker{}
	h.deps.Waker = wk
	id := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	for i, modern := range []bool{false, true} {
		cursor := int64(1 + i)
		later(300*time.Millisecond, func() {
			e.event(id, store.KindArtifact, &a2a.TaskArtifactUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: "ctx",
				Artifact: &a2a.Artifact{ID: "a1", Name: "draft", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}})
			wk.wake()
		})
		w := progressCall(t, h, e.buyer, modern, ToolStatus, map[string]any{"taskId": id.String(), "after": cursor, "wait": 10}, "tok-1")
		msgs := progressStream(t, w)
		if len(msgs) != 2 || msgs[0]["method"] != "notifications/progress" || msgs[1]["result"] == nil {
			t.Fatalf("modern %v: the stream: %s", modern, w.Body)
		}
		params := msgs[0]["params"].(map[string]any)
		if params["progressToken"] != "tok-1" || params["progress"] != 1.0 || params["message"] != "artifact draft" {
			t.Errorf("modern %v: the progress notification: %v", modern, params)
		}
	}
}

// existingHirer answers every hire with an existing task (a retry, a keyed hire re-issued).
type existingHirer struct {
	replyRecorder
	row *store.TaskRow
}

func (x *existingHirer) Hire(context.Context, HireRequest) (HireOutcome, error) {
	return HireOutcome{Task: x.row, Existing: true}, nil
}

// A hire answered with an existing task (a keyed hire re-issued after its client reconnects) sends progress for the
// rows that land from now, not the task's history (D339).
func TestHireOfAnExistingTaskSendsNoHistoryAsProgress(t *testing.T) {
	e := newModernEnv(t)
	h := toolsHandler(e, nil)
	id := e.task(e.buyer.BuyerID, store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), 0, true)
	for _, text := range []string{"step one", "step two"} {
		e.event(id, store.KindStatus, &a2a.TaskStatusUpdateEvent{TaskID: a2a.TaskID(id.String()), ContextID: "ctx",
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: agentText(text)}})
	}
	if _, err := e.pool.Exec(context.Background(), `update kontra.tasks set state = 'completed', terminal_at = now() where id = $1`, id); err != nil {
		t.Fatal(err)
	}
	row, err := store.NewLog(e.pool).Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	h.deps.Hirer = &existingHirer{row: row}
	for _, modern := range []bool{false, true} {
		w := progressCall(t, h, e.buyer, modern, ToolHire, map[string]any{"service": "hello", "message": "book it",
			"idempotencyKey": "k1"}, "tok-h")
		msgs := progressStream(t, w)
		if len(msgs) == 0 || msgs[len(msgs)-1]["result"] == nil || !strings.Contains(w.Body.String(), "TASK_STATE_COMPLETED") {
			t.Fatalf("modern %v: the hire's answer: %s", modern, w.Body)
		}
		for _, m := range msgs {
			if m["method"] == "notifications/progress" {
				t.Errorf("modern %v: the task's history sent as progress: %v", modern, m["params"])
			}
		}
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

// The tasks tool's bound holds under the generic plan Postgres may give its prepared statement after five executions
// (D333): a page after a deep cursor reads about HiredReadBound rows, not the cursor's depth more. With a
// "$2 is null or (status_at, id) < ($2, $3)" predicate the generic plan's index condition is the buyer alone and the
// keyset a filter, so the read below cost the 1500 rows above the cursor too.
func TestTasksDeepCursorUnderAGenericPlan(t *testing.T) {
	e := newModernEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id,
			state, version, snapshot, status_at, submitted_at, hired)
		select gen_random_uuid(), $1, 'hello', 'a2a', $2, 'key:kk_buyer', 'api_key', 'ctx', 'working', 1, '{"id":"x"}',
			now() - i * interval '1 second', now(), true
		from generate_series(1, 2600) i`, e.team, e.buyer.BuyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `analyze kontra.tasks`); err != nil {
		t.Fatal(err)
	}
	var (
		at time.Time
		id uuid.UUID
	)
	if err := e.pool.QueryRow(ctx, `select status_at, id from kontra.tasks where buyer_id = $1 and hired
		order by status_at desc, id desc offset 1500 limit 1`, e.buyer.BuyerID).Scan(&at, &id); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(e.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "1")
	q.Set("plan_cache_mode", "force_generic_plan")
	u.RawQuery = q.Encode()
	pool := pgtest.Pool(t, u.String())
	log := store.NewLog(pool)
	read := func() int64 {
		t.Helper()
		var n int64
		if _, err := pool.Exec(ctx, `select pg_stat_force_next_flush()`); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `select coalesce(sum(idx_tup_read), 0)::bigint + coalesce(max(seq_tup_read), 0)
			from pg_stat_user_indexes i join pg_stat_user_tables s using (relid) where s.schemaname = 'kontra' and s.relname = 'tasks'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	cursor := store.EncodePageToken(at, id)
	f := store.HiredFilter{States: []store.State{store.StateCompleted}}
	for i := range 7 {
		before := read()
		rows, next, err := log.ListHiredFiltered(ctx, e.buyer.BuyerID, f, cursor)
		if err != nil || len(rows) != 0 || next == "" {
			t.Fatalf("read %d: %d rows, next %q, %v", i, len(rows), next, err)
		}
		if n := read() - before; n > store.HiredReadBound+100 {
			t.Fatalf("read %d after a cursor 1500 rows deep read %d rows, more than the bound %d", i, n, store.HiredReadBound)
		}
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
// every hire of it within one period and differs for another key or principal; the next period's hire looks for the
// period before's messageId too, and the period after that mints a messageId neither of the first's (a key reused a
// day later is another task with another messageId, which no seller can link to the first); the key's lock is the
// same in every period; a key the Hirer finds used for other arguments is isError and creates nothing; a malformed key
// breaks the input schema.
func TestHireIdempotencyKeyArguments(t *testing.T) {
	team := uuid.Must(uuid.NewV7())
	p := keyPrincipal(team, "default", "kk_a")
	q := keyPrincipal(team, "default", "kk_b")
	now := time.Date(2026, 11, 5, 23, 59, 0, 0, time.UTC)
	k := HireKeyOf(p, "k1", now)
	if len(k.MessageIDs) != 3 || HireKeyOf(p, "k1", now.Add(-time.Hour)).MessageIDs[0] != k.MessageIDs[0] ||
		HireKeyOf(p, "k2", now).MessageIDs[0] == k.MessageIDs[0] || HireKeyOf(q, "k1", now).MessageIDs[0] == k.MessageIDs[0] {
		t.Fatalf("one period's messageIds: %+v", k)
	}
	next := HireKeyOf(p, "k1", now.Add(2*time.Minute))
	if next.MessageIDs[1] != k.MessageIDs[0] || k.MessageIDs[2] != next.MessageIDs[0] || next.MessageIDs[0] == k.MessageIDs[0] ||
		next.Lock != k.Lock {
		t.Errorf("the next period does not find the period before's task, or locks another key: %+v, %+v", next, k)
	}
	day := HireKeyOf(p, "k1", now.Add(25*time.Hour))
	if slices.Contains(day.MessageIDs, k.MessageIDs[0]) || day.Lock != k.Lock {
		t.Errorf("a key reused after the window mints the first hire's messageId again: %+v, %+v", day, k)
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
