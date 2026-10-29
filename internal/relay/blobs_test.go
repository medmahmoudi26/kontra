package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/blob/blobtest"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	ssew "kontra.sh/kontra/internal/sse"
	"kontra.sh/kontra/internal/store"
)

// recordingSeller records the message of each turn it executes, then runs then (completed with "ok" when nil).
type recordingSeller struct {
	mu   sync.Mutex
	got  []*a2a.Message
	then func(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool)
}

func (s *recordingSeller) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		s.mu.Lock()
		s.got = append(s.got, ec.Message)
		then := s.then
		s.mu.Unlock()
		if then != nil {
			then(ec, yield)
			return
		}
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("ok"))), nil)
	}
}

func (s *recordingSeller) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(nil, a2a.ErrTaskNotCancelable) }
}

func (s *recordingSeller) messages() []*a2a.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*a2a.Message(nil), s.got...)
}

// fetch GETs a presigned URL and returns its body.
func fetch(t *testing.T, u string) []byte {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	return b
}

// presigned returns the URL of a part rendered for a caller, failing unless it is a presigned GET of the bucket.
func presigned(t *testing.T, bt *blobtest.Server, p *a2a.Part) string {
	t.Helper()
	u, ok := p.Content.(a2a.URL)
	if !ok || !strings.HasPrefix(string(u), bt.Endpoint+"/"+blobtest.Bucket+"/") || !strings.Contains(string(u), "X-Amz-Signature=") {
		t.Fatalf("part %v is not a presigned GET of the bucket", p.Content)
	}
	return string(u)
}

// blobRows reads the blobs rows of a task.
func (e *env) blobRows(id a2a.TaskID) []blob.Row {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `select ref, tenant_id, task_id, size from kontra.blobs where task_id = $1 order by created_at`, uid(e.t, id))
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []blob.Row
	for rows.Next() {
		var r blob.Row
		var ref string
		if err := rows.Scan(&ref, &r.Tenant, &r.Task, &r.Size); err != nil {
			e.t.Fatal(err)
		}
		r.Ref = blob.Ref(ref)
		out = append(out, r)
	}
	return out
}

// creationMessage is the caller's first message as the task's creation row (seq 1) logged it.
func (e *env) creationMessage(id a2a.TaskID) *a2a.Message {
	e.t.Helper()
	evs := e.events(id)
	var sr a2a.StreamResponse
	if len(evs) == 0 || evs[0].Kind != store.KindTask || json.Unmarshal(evs[0].Payload, &sr) != nil {
		e.t.Fatalf("no creation row: %+v", evs)
	}
	t, ok := sr.Event.(*a2a.Task)
	if !ok || len(t.History) != 1 {
		e.t.Fatalf("creation row %s", evs[0].Payload)
	}
	return t.History[0]
}

// A caller part over 256 KiB on a gate with a bucket (spec §10.4, D167): it is stored as an object of the task
// and logged as a url part kontra-blob:<ref> with its media type and filename, its blobs row written with the
// task's creation row; the direct backend gets a presigned GET that fetches the same bytes; GetTask, the
// SendMessage answer and ListTasks render it as a presigned GET for the caller. Nothing of the bytes is in the
// log.
func TestCallerPartGoesToTheBucket(t *testing.T) {
	bt := blobtest.New(t, "parts")
	e := newEnv(t, envOptions{bucket: bt.Store})
	seller := &recordingSeller{}
	_, b, info := newAgent(t, seller, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)

	data := bytes.Repeat([]byte("0123456789abcdef"), 300<<10/16) // 300 KiB
	big := &a2a.Part{Content: a2a.Raw(data), MediaType: "application/pdf", Filename: "brief.pdf"}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("read this"), big)
	res, err := e.handler().SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("SendMessage = %+v", res)
	}
	e.queue.wait(t)
	id := task.ID

	// The log holds the gate's name, the row and the object exist.
	logged := e.creationMessage(id)
	u, _ := logged.Parts[1].Content.(a2a.URL)
	ref, isRef := blob.RefOf(string(u))
	if !isRef || logged.Parts[1].MediaType != "application/pdf" || logged.Parts[1].Filename != "brief.pdf" || logged.Parts[0].Text() != "read this" {
		t.Fatalf("logged parts %+v %+v", logged.Parts[0], logged.Parts[1])
	}
	rows := e.blobRows(id)
	if len(rows) != 1 || rows[0].Ref != ref || rows[0].Size != int64(len(data)) || rows[0].Tenant != e.svc.TenantID {
		t.Fatalf("blobs rows %+v, want %s of %d bytes", rows, ref, len(data))
	}
	if keys := bt.Keys(t); len(keys) != 1 || keys[0] != "parts/"+e.svc.TenantID.String()+"/"+string(id)+"/"+string(ref) {
		t.Fatalf("objects %v", keys)
	}
	if text := e.logText(id); strings.Contains(text, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3") || len(text) > 64<<10 {
		t.Fatalf("the part's bytes reached the log (%d bytes of log)", len(text))
	}
	// The direct backend got a presigned GET of the same bytes.
	sent := seller.messages()
	if len(sent) != 1 || len(sent[0].Parts) != 2 {
		t.Fatalf("the seller got %+v", sent)
	}
	if got := fetch(t, presigned(t, bt, sent[0].Parts[1])); !bytes.Equal(got, data) {
		t.Fatal("the backend's presigned GET does not fetch the part's bytes")
	}
	if sent[0].Parts[1].MediaType != "application/pdf" {
		t.Errorf("the backend's part lost its media type: %+v", sent[0].Parts[1])
	}
	// The caller's answers render it.
	for name, history := range map[string][]*a2a.Message{"SendMessage": task.History} {
		if len(history) == 0 || len(history[0].Parts) != 2 || !bytes.Equal(fetch(t, presigned(t, bt, history[0].Parts[1])), data) {
			t.Errorf("%s: history %+v", name, history)
		}
	}
	hl := 10
	got, err := e.handler().GetTask(asCaller(buyerA), &a2a.GetTaskRequest{ID: id, HistoryLength: &hl})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fetch(t, presigned(t, bt, got.History[0].Parts[1])), data) {
		t.Error("GetTask: the presigned GET does not fetch the part's bytes")
	}
	list, err := e.handler().ListTasks(asCaller(buyerA), &a2a.ListTasksRequest{HistoryLength: &hl})
	if err != nil || len(list.Tasks) != 1 {
		t.Fatalf("ListTasks: %+v %v", list, err)
	}
	if h := list.Tasks[0].History; len(h) > 0 {
		presigned(t, bt, h[0].Parts[1])
	}
	// The stored projection still names the object.
	if p := projection(t, e, id); p.History[0].Parts[1].Content != a2a.URL(ref.URL()) {
		t.Errorf("the projection holds %v", p.History[0].Parts[1].Content)
	}
}

// A follow-up's large part is stored the same way, its row written with its work item (the message's own row
// is logged by the turn's dispatch).
func TestFollowUpPartGoesToTheBucket(t *testing.T) {
	bt := blobtest.New(t, "")
	e := newEnv(t, envOptions{bucket: bt.Store})
	_, b, info := newAgent(t, askingAgent(), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	h := e.handler()
	res, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("book"))})
	if err != nil {
		t.Fatal(err)
	}
	e.queue.wait(t)
	task := res.(*a2a.Task)
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("turn 1: %s", task.Status.State)
	}
	data := []byte(strings.Repeat("é", 200<<10)) // 400 KiB of UTF-8
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(string(data)))
	m.TaskID = task.ID
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: m}); err != nil {
		t.Fatal(err)
	}
	e.queue.wait(t)
	rows := e.blobRows(task.ID)
	if len(rows) != 1 || rows[0].Size != int64(len(data)) {
		t.Fatalf("blobs rows %+v", rows)
	}
	var found bool
	for _, ev := range e.events(task.ID) {
		if ev.Kind == store.KindMessage && bytes.Contains(ev.Payload, []byte(rows[0].Ref.URL())) && bytes.Contains(ev.Payload, []byte("text/plain; charset=utf-8")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no message row names %s", rows[0].Ref.URL())
	}
	rc, _, err := bt.Store.Open(context.Background(), rows[0].Key())
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(stored, data) {
		t.Fatal("the object does not hold the follow-up's text")
	}
}

// kontra-blob: is the gate's (D181): a caller's url part in that scheme, in any case, is InvalidRequest on both
// bindings and creates no task, even when it names an object of another task, and whether or not the gate has a
// bucket.
func TestCallerReservedSchemeRefused(t *testing.T) {
	bt := blobtest.New(t, "")
	e := newEnv(t, envOptions{bucket: bt.Store})
	victim, _ := e.newTask(taskSpec{owner: buyerB, text: "mine"})
	_, row, err := blob.Externalize(context.Background(), bt.Store, e.svc.TenantID, uid(t, victim), &a2a.Part{Content: a2a.Raw(bytes.Repeat([]byte{1}, 300<<10))})
	if err != nil {
		t.Fatal(err)
	}
	if err := blob.Insert(context.Background(), e.pool, []blob.Row{row}); err != nil {
		t.Fatal(err)
	}
	h := e.handler()
	withCaller := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal(buyerA))))
		})
	}
	rpc := httptest.NewServer(withCaller(a2asrv.NewJSONRPCHandler(h)))
	defer rpc.Close()
	rest := httptest.NewServer(withCaller(a2asrv.NewRESTHandler(h)))
	defer rest.Close()
	tasks := e.countRows("tasks")
	for _, u := range []string{row.Ref.URL(), "KONTRA-BLOB:" + string(row.Ref), "kontra-blob:anything"} {
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("steal"), &a2a.Part{Content: a2a.URL(u)})
		params, _ := json.Marshal(a2a.SendMessageRequest{Message: msg})
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": json.RawMessage(params)})
		req, _ := http.NewRequest(http.MethodPost, rpc.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if out.Error == nil || out.Error.Code != -32600 {
			t.Errorf("JSON-RPC with %q: %+v, want -32600", u, out.Error)
		}
		req, _ = http.NewRequest(http.MethodPost, rest.URL+"/message:send", bytes.NewReader(params))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("HTTP+JSON with %q: %d %s, want 400", u, resp.StatusCode, b)
		}
		// And straight through the handler, the error is a2a-go's InvalidRequest.
		if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: msg}); !errors.Is(err, a2a.ErrInvalidRequest) {
			t.Errorf("SendMessage with %q: %v", u, err)
		}
	}
	if n := e.countRows("tasks"); n != tasks {
		t.Errorf("a refused request created %d tasks", n-tasks)
	}
	// Without a bucket too.
	e2 := newEnv(t, envOptions{})
	msg := a2a.NewMessage(a2a.MessageRoleUser, &a2a.Part{Content: a2a.URL(row.Ref.URL())})
	if _, err := e2.handler().SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: msg}); !errors.Is(err, a2a.ErrInvalidRequest) {
		t.Errorf("without a bucket: %v", err)
	}
}

// A service's url part in the reserved scheme fails the task upstream_error (D181), and nothing of it is logged.
func TestUpstreamReservedSchemeFails(t *testing.T) {
	e := newEnv(t, envOptions{})
	seller := &recordingSeller{then: func(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewArtifactEvent(ec, &a2a.Part{Content: a2a.URL("kontra-blob:aaaaaaaaaaaaaaaaaaaaaaaaaa")}), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}}
	_, b, info := newAgent(t, seller, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "upstream_error" || len(p.Artifacts) != 0 ||
		!strings.Contains(p.Status.Message.Parts[0].Text(), "kontra-blob") {
		t.Fatalf("%+v %+v", res, p.Status)
	}
	if strings.Contains(e.logText(id), "aaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Error("the reserved part reached the log")
	}
}

// An upstream part over 256 KiB on a gate with a bucket (spec §10.4) is stored as an object and logged as its
// url part, its row in the event's own transaction, and the task completes instead of failing upstream_error.
// GetTask and the gate's SSE writer render it as a presigned GET that fetches the same bytes.
func TestUpstreamPartGoesToTheBucket(t *testing.T) {
	bt := blobtest.New(t, "")
	e := newEnv(t, envOptions{bucket: bt.Store, opts: ExecutorOptions{TaskBytes: 400 << 10}})
	data := bytes.Repeat([]byte("artifact"), 300<<10/8) // 300 KiB
	seller := &recordingSeller{then: func(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewArtifactEvent(ec, &a2a.Part{Content: a2a.Raw(data), MediaType: "application/zip", Filename: "out.zip"}), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}}
	_, b, info := newAgent(t, seller, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "zip it"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || len(p.Artifacts) != 1 {
		t.Fatalf("%+v %+v %v", res, p.Status, p.Metadata)
	}
	part := p.Artifacts[0].Parts[0]
	ref, ok := blob.RefOf(string(part.Content.(a2a.URL)))
	if !ok || part.MediaType != "application/zip" || part.Filename != "out.zip" {
		t.Fatalf("logged part %+v", part)
	}
	rows := e.blobRows(id)
	if len(rows) != 1 || rows[0].Ref != ref || rows[0].Size != int64(len(data)) {
		t.Fatalf("blobs rows %+v", rows)
	}
	// The row came with the artifact's event: every row's ref is named by an event logged no later than it.
	var artifactAt time.Time
	for _, ev := range e.events(id) {
		if ev.Kind == store.KindArtifact {
			artifactAt = ev.CreatedAt
		}
	}
	var rowAt time.Time
	if err := e.pool.QueryRow(context.Background(), `select created_at from kontra.blobs where ref = $1`, string(ref)).Scan(&rowAt); err != nil {
		t.Fatal(err)
	}
	if !rowAt.Equal(artifactAt) {
		t.Errorf("the blobs row (%v) was not written in the artifact's transaction (%v)", rowAt, artifactAt)
	}
	got, err := e.handler().GetTask(asCaller(buyerA), &a2a.GetTaskRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fetch(t, presigned(t, bt, got.Artifacts[0].Parts[0])), data) {
		t.Fatal("GetTask: the presigned GET does not fetch the artifact's bytes")
	}
	// The gate's SSE writer: a resumed stream renders the artifact row.
	w := ssew.NewWriterWithOptions(e.log, nil, ssew.Options{Render: e.render})
	one := int64(1)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agents/hello", nil)
	w.Subscribe(rec, req, ssew.Stream{Binding: ssew.HTTPJSON, Service: e.svc, Owner: buyerA, TaskID: id, LastEventID: &one})
	var streamed []string
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if d, ok := strings.CutPrefix(line, "data: "); ok && strings.Contains(d, "artifactUpdate") {
			streamed = append(streamed, d)
		}
	}
	if len(streamed) != 1 {
		t.Fatalf("SSE: %d artifact events in %q", len(streamed), rec.Body.String())
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal([]byte(streamed[0]), &sr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fetch(t, presigned(t, bt, sr.Event.(*a2a.TaskArtifactUpdateEvent).Artifact.Parts[0])), data) {
		t.Fatal("SSE: the presigned GET does not fetch the artifact's bytes")
	}
}

// A polled upstream that shows a large artifact in several GetTask answers before it completes: the artifact is
// stored and logged once, not at every poll (the object's digest matches the projection's part).
func TestPolledLargeArtifactLoggedOnce(t *testing.T) {
	bt := blobtest.New(t, "")
	e := newEnv(t, envOptions{bucket: bt.Store})
	data := bytes.Repeat([]byte("p"), 300<<10)
	seller := &recordingSeller{then: func(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		if !yield(a2a.NewArtifactEvent(ec, &a2a.Part{Content: a2a.Raw(data)}), nil) {
			return
		}
		time.Sleep(600 * time.Millisecond) // several 50 ms polls see the artifact
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}}
	_, b, info := newAgent(t, seller, agentConfig{})
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "slow"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if res.Outcome != lifecycle.DispatchTerminal || projection(t, e, id).Status.State != a2a.TaskStateCompleted {
		t.Fatalf("%+v %v", res, e.states(id))
	}
	n := 0
	for _, s := range e.states(id) {
		if s == "artifact" {
			n++
		}
	}
	if n != 1 || len(bt.Keys(t)) != 1 || len(e.blobRows(id)) != 1 {
		t.Fatalf("%d artifact rows, %d objects, %d blobs rows: want one each (log %v)", n, len(bt.Keys(t)), len(e.blobRows(id)), e.states(id))
	}
}

// A dispatch to a direct backend carries a presigned GET; a tunnel dispatch carries kontra-blob:<ref>, which the
// node fetches on a blob stream (spec §10.4, tunnel.md §8.3).
func TestTunnelDispatchKeepsRefs(t *testing.T) {
	bt := blobtest.New(t, "")
	s1 := newSession()
	seller := &recordingSeller{}
	e, _, _ := tunnelEnv(t, envOptions{bucket: bt.Store}, true, seller, s1)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "x", session: &s1})
	// The work item's message names an object of the task, as the interceptors write it.
	_, row, err := blob.Externalize(context.Background(), bt.Store, e.svc.TenantID, uid(t, id), &a2a.Part{Content: a2a.Raw(bytes.Repeat([]byte{9}, 300<<10))})
	if err != nil {
		t.Fatal(err)
	}
	if err := blob.Insert(context.Background(), e.pool, []blob.Row{row}); err != nil {
		t.Fatal(err)
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, &a2a.Part{Content: a2a.URL(row.Ref.URL())})
	e.exec(`update kontra.work_items set payload = $2::json where id = $1`, wid, mustPayload(t, msg, buyerA))
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("%+v", res)
	}
	got := seller.messages()
	if len(got) != 1 || got[0].Parts[0].Content != a2a.URL(row.Ref.URL()) {
		t.Fatalf("the node's service got %+v, want kontra-blob:<ref>", got)
	}
}

func mustPayload(t *testing.T, msg *a2a.Message, o store.Owner) string {
	t.Helper()
	b, err := store.MarshalPayload(store.WorkPayload{Request: &a2a.SendMessageRequest{Message: msg}, CallContext: callContext(o, nil), Owner: o})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// bigPart is a caller part over the inline limit.
func bigPart(b byte) *a2a.Part {
	return &a2a.Part{Content: a2a.Raw(bytes.Repeat([]byte{b}, 300<<10)), MediaType: "application/octet-stream"}
}

// objectsOf lists the bucket's objects under a task.
func objectsOf(t *testing.T, bt *blobtest.Server, task a2a.TaskID) []string {
	t.Helper()
	var out []string
	for _, k := range bt.Keys(t) {
		if strings.Contains(k, "/"+string(task)+"/") {
			out = append(out, k)
		}
	}
	return out
}

// A new task's large parts go to the bucket only once nothing before the work queue can refuse the request, and
// a request refused anyway, or answered with another task, leaves no object behind (spec §5.4, §10.4: objects die
// with their task, and a refused request has none): an inline push URL the SSRF rules refuse, a message a2a-go
// refuses after the interceptors, a work queue that fails, and two identical requests of which the second is
// answered with the first's task (the resend race: both passed the interceptors' resend check).
func TestRefusedNewTaskLeavesNoObjects(t *testing.T) {
	bt := blobtest.New(t, "")
	e := newEnv(t, envOptions{bucket: bt.Store})
	h := e.handler()
	tasks := e.countRows("tasks")

	// The push URL is refused after Admission.
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("read this"), bigPart(1))
	_, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: msg,
		Config: &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://10.0.0.1/hook"}}})
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("a private push URL: %v", err)
	}
	if keys := bt.Keys(t); len(keys) != 0 {
		t.Errorf("a request refused for its push URL left objects %v", keys)
	}

	// a2a-go refuses a message without an id after every interceptor ran.
	msg = a2a.NewMessage(a2a.MessageRoleUser, bigPart(2))
	msg.ID = ""
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: msg}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("a message without an id: %v", err)
	}
	if keys := bt.Keys(t); len(keys) != 0 {
		t.Errorf("a request a2a-go refused left objects %v", keys)
	}

	// The work queue fails.
	e.queue.failNew = errors.New("the database is gone")
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, bigPart(3))}); err == nil {
		t.Fatal("a failed work queue answered")
	}
	e.queue.failNew = nil
	if keys := bt.Keys(t); len(keys) != 0 {
		t.Errorf("a request the work queue failed left objects %v", keys)
	}
	if n := e.countRows("tasks"); n != tasks {
		t.Fatalf("refused requests created %d tasks", n-tasks)
	}

	// The resend race: the first request created its task; the second, identical, stored its own objects under
	// the task Admission pre-minted for it, and the work queue answers it with the first task.
	seller := &recordingSeller{}
	_, b, info := newAgent(t, seller, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	first := a2a.NewMessage(a2a.MessageRoleUser, bigPart(4))
	res, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: first})
	if err != nil {
		t.Fatal(err)
	}
	e.queue.wait(t)
	kept := res.(*a2a.Task).ID
	before := objectsOf(t, bt, kept)
	if len(before) != 1 {
		t.Fatalf("the first task's objects: %v", before)
	}
	// The race itself cannot be timed here: a second request that passed the interceptors' resend check (another
	// message id, so it does) is answered by the work queue with the first task, as lifecycle.Queue answers the
	// later of two identical requests under the resend key's lock.
	e.queue.resendTo = kept
	res, err = h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, bigPart(4))})
	e.queue.resendTo = ""
	if err != nil {
		t.Fatalf("the resend: %v", err)
	}
	if got := res.(*a2a.Task).ID; got != kept {
		t.Fatalf("the resend was answered with %s, want %s", got, kept)
	}
	if keys := bt.Keys(t); len(keys) != 1 || keys[0] != before[0] {
		t.Errorf("objects after the resend race: %v, want only the first task's %v", keys, before)
	}
	if rows := e.blobRows(kept); len(rows) != 1 {
		t.Errorf("the first task's blobs rows: %+v", rows)
	}
}

// A follow-up's or a review-window message's large parts are stored once the task is known to take the message,
// and a message refused afterwards leaves neither objects nor blobs rows (spec §7, §10.4: a refused follow-up's
// content is not kept; per-task totals are bounded): a review message the window refuses, rejects, or whose
// caller metadata is over 16 KiB; a follow-up whose new push URL is refused; a follow-up the workflow refuses. A
// review message not accepted in time keeps its work item, and so its objects (D66); an accepted one is
// appended, naming them.
func TestRefusedMessagesLeaveNoObjects(t *testing.T) {
	bt := blobtest.New(t, "")
	fr := &fakeReviews{}
	e := newEnv(t, envOptions{bucket: bt.Store, reviews: fr, rules: hooks.KeyRules{CallerMetadata: []string{"kontra.escrow.dispute"}}})
	fr.e = e
	h := e.handler()
	done, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.setState(done, a2a.TaskStateCompleted)
	send := func(id a2a.TaskID, b byte, md map[string]any, cfg *a2a.SendMessageConfig) error {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("about this"), bigPart(b))
		m.TaskID, m.Metadata = id, md
		_, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: m, Config: cfg})
		return err
	}
	clean := func(what string, id a2a.TaskID) {
		t.Helper()
		if keys := objectsOf(t, bt, id); len(keys) != 0 {
			t.Errorf("%s: objects %v left", what, keys)
		}
		if rows := e.blobRows(id); len(rows) != 0 {
			t.Errorf("%s: blobs rows %+v left", what, rows)
		}
	}
	for i, mode := range []string{"refuse", "reject"} {
		fr.mode = mode
		if err := send(done, byte(i), nil, nil); !errors.Is(err, a2a.ErrUnsupportedOperation) {
			t.Fatalf("%s: %v", mode, err)
		}
		clean("a review message the window "+mode+"s", done)
	}
	fr.mode = "accept"
	if err := send(done, 5, map[string]any{"kontra.escrow.dispute": strings.Repeat("x", 17<<10)}, nil); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Fatalf("caller metadata over 16 KiB: %v", err)
	}
	clean("a review message with too much caller metadata", done)
	if len(fr.got) != 2 {
		t.Fatalf("the window was asked %d times", len(fr.got))
	}

	// Not accepted in time: the work item is kept, so are its objects.
	fr.mode = "timeout"
	if err := send(done, 6, nil, nil); !errors.Is(err, a2a.ErrInternalError) {
		t.Fatalf("timeout: %v", err)
	}
	if keys, rows := objectsOf(t, bt, done), e.blobRows(done); len(keys) != 1 || len(rows) != 1 {
		t.Fatalf("a review message not accepted in time: objects %v, rows %+v (want its one, kept with its work item)", keys, rows)
	}
	// Accepted: appended, naming its object.
	fr.mode = "accept"
	if err := send(done, 7, nil, nil); err != nil {
		t.Fatal(err)
	}
	rows := e.blobRows(done)
	if keys := objectsOf(t, bt, done); len(keys) != 2 || len(rows) != 2 {
		t.Fatalf("an accepted review message: objects %v, rows %+v", keys, rows)
	}
	if !strings.Contains(e.logText(done), rows[1].Ref.URL()) {
		t.Error("the accepted message's row names no logged ref")
	}

	// Follow-ups to a task waiting for input.
	waiting, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.setState(waiting, a2a.TaskStateInputRequired)
	if err := send(waiting, 8, nil, &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://10.0.0.2/hook"}}); err == nil {
		t.Fatal("a follow-up with a private push URL was taken")
	}
	clean("a follow-up refused for its push URL", waiting)
	e.queue.refuseFollowUp = true
	if err := send(waiting, 9, nil, nil); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Fatalf("a refused follow-up: %v", err)
	}
	e.queue.refuseFollowUp = false
	clean("a follow-up the workflow refused", waiting)
}

// Objects count toward limits.task_bytes (spec §6.6 "nothing an upstream sends is unbounded", §10.4 "per-task
// totals are bounded"): a seller that streams large artifacts fails the task upstream_error once the objects it
// stored and the events logged would exceed the limit, the object that would cross it never written; and a
// dispatch starts from the objects the task already holds (the caller's too, by their blobs rows).
func TestObjectsCountTowardTaskBytes(t *testing.T) {
	bt := blobtest.New(t, "")
	e := newEnv(t, envOptions{bucket: bt.Store, opts: ExecutorOptions{TaskBytes: 1 << 20}})
	chunk := bytes.Repeat([]byte("z"), 300<<10)
	seller := &recordingSeller{then: func(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		for range 20 {
			if !yield(a2a.NewArtifactEvent(ec, &a2a.Part{Content: a2a.Raw(chunk)}), nil) {
				return
			}
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}}
	_, b, info := newAgent(t, seller, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "stream"})
	res := e.dispatch(context.Background(), id, 1, wid, false)
	p := projection(t, e, id)
	if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateFailed || p.Metadata[store.MetaFailure] != "upstream_error" ||
		!strings.Contains(p.Status.Message.Parts[0].Text(), "limits.task_bytes") {
		t.Fatalf("a seller storing 20 objects of 300 KiB under a 1 MiB task_bytes: %+v %v %v", res, p.Status.State, p.Metadata)
	}
	if keys := objectsOf(t, bt, id); len(keys) != 3 || len(e.blobRows(id)) != 3 {
		t.Fatalf("objects %v, rows %d: want the three that fit in 1 MiB", keys, len(e.blobRows(id)))
	}

	// The caller's objects count: a task whose first message holds a 300 KiB object has 212 KiB left of 512 KiB,
	// and a 300 KiB artifact fails it.
	e2 := newEnv(t, envOptions{bucket: bt.Store, opts: ExecutorOptions{TaskBytes: 512 << 10}})
	one := &recordingSeller{then: func(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewArtifactEvent(ec, &a2a.Part{Content: a2a.Raw(chunk)}), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}}
	_, b2, info2 := newAgent(t, one, agentConfig{streaming: true})
	info2.Streaming = true
	e2.backends.set(b2, info2, nil)
	id2, wid2 := e2.newTask(taskSpec{owner: buyerA, text: "with an attachment"})
	_, row, err := blob.Externalize(context.Background(), bt.Store, e2.svc.TenantID, uid(t, id2), bigPart(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := blob.Insert(context.Background(), e2.pool, []blob.Row{row}); err != nil {
		t.Fatal(err)
	}
	e2.dispatch(context.Background(), id2, 1, wid2, false)
	if p := projection(t, e2, id2); p.Status.State != a2a.TaskStateFailed || p.Metadata[store.MetaFailure] != "upstream_error" {
		t.Fatalf("an artifact over what the caller's object left: %v %v", p.Status.State, p.Metadata)
	}
	if keys := objectsOf(t, bt, id2); len(keys) != 1 {
		t.Errorf("objects %v: the artifact's object was written although it did not fit", keys)
	}
}
