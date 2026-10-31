package gatemcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
)

// The fixes of M4's review on /mcp (relay.md §12, D303).

// followRecorder is a Hirer whose Reply records the follow-up and appends it to the task's log as the work queue
// would (a message event; the task stays input-required until its turn ends), answering the version before it.
type followRecorder struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	got  []string
}

func (f *followRecorder) Hire(context.Context, HireRequest) (HireOutcome, error) {
	return HireOutcome{}, errStub
}

func (f *followRecorder) Reply(ctx context.Context, p auth.Principal, task *store.TaskRow, parts a2a.ContentParts) (int64, error) {
	f.mu.Lock()
	f.got = append(f.got, task.ID.String()+":"+parts[0].Text())
	f.mu.Unlock()
	_, err := f.pool.Exec(ctx, `with e as (insert into kontra.task_events (task_id, seq, kind, payload)
			select id, version + 1, 'message', '{}' from kontra.tasks where id = $1 returning seq)
		update kontra.tasks set version = (select seq from e) where id = $1`, task.ID)
	return task.Version, err
}

func (f *followRecorder) replies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

var errStub = errors.New("not in this test")

// complete ends task id's turn as its dispatch would: a status event, the row completed with the agent's answer.
func complete(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, answer string) {
	t.Helper()
	task := taskWith(a2a.TaskStateCompleted, agentText(answer), nil)
	task.ID = a2a.TaskID(id.String())
	snap, _ := json.Marshal(task)
	if _, err := pool.Exec(context.Background(), `with e as (insert into kontra.task_events (task_id, seq, kind, payload)
			select id, version + 1, 'status', '{}' from kontra.tasks where id = $1 returning seq)
		update kontra.tasks set state = 'completed', version = (select seq from e), snapshot = $2::json, terminal_at = now(),
			status_at = now() where id = $1`, id, string(snap)); err != nil {
		t.Fatal(err)
	}
}

// legacyCall is a legacy tools/call of tool with args by p, as the session handler passes it to toolsCall.
func legacyCall(t *testing.T, p auth.Principal, tool string, args map[string]any) (*request, *httptest.ResponseRecorder) {
	t.Helper()
	rq, w := modernRequest(t, "tools/call", map[string]any{"name": tool, "arguments": args}, nil)
	rq.p, rq.modern, rq.start = p, false, time.Now()
	return rq, w
}

func toolAnswer(t *testing.T, w *httptest.ResponseRecorder) (rendered, answer) {
	t.Helper()
	a := answerOf(t, w)
	var r rendered
	if a.Error == nil {
		if err := json.Unmarshal(a.Result, &r); err != nil {
			t.Fatalf("%s: %v", a.body, err)
		}
	}
	return r, a
}

// MCP-2: a reply's plain wait reads the task's row and the seq of the event its state comes from in one statement.
// The turn's terminal event committed between two separate reads (here, right after the wait's read) used to end the
// wait on the row's earlier question: the reply answered "Which date?" while the task had completed, and the client's
// next reply was refused. Now the wait goes on, and the reply answers the completed turn.
func TestReplyAnswersTheTurnItStarted(t *testing.T) {
	e := newModernEnv(t)
	hirer := &followRecorder{pool: e.pool}
	id := e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil), 2, true)
	var reads atomic.Int32
	h := &Handler{deps: Deps{PublicURL: "https://gate.example", Hirer: hirer, Tasks: e.tasks, Log: store.NewLog(e.pool),
		Waker: noWake{}, PlainWait: 10 * time.Second,
		afterRead: func(row *store.TaskRow) {
			// The first read after the follow-up: the row still at input-required (version 4, its message), and the turn's
			// terminal event committed now, before the wait decides.
			if reads.Add(1) == 1 && row.State == store.StateInputRequired && row.Version == 4 {
				complete(t, e.pool, row.ID, "booked for tomorrow")
			}
		}}, log: slog.New(slog.DiscardHandler)}
	rq, w := legacyCall(t, e.buyer, ToolReply, map[string]any{"taskId": id.String(), "message": "tomorrow"})
	h.toolsCall(rq)
	r, a := toolAnswer(t, w)
	if reads.Load() < 2 {
		t.Fatalf("the wait read the task %d times", reads.Load())
	}
	if a.Error != nil || r.IsError || len(r.Content) != 1 || r.Content[0].Text != "booked for tomorrow" ||
		!strings.Contains(string(r.StructuredContent["state"]), "TASK_STATE_COMPLETED") {
		t.Fatalf("the reply answered %s, want the completed turn", a.body)
	}
	if got := hirer.replies(); len(got) != 1 {
		t.Fatalf("follow-ups %v", got)
	}
}

// servedMap is a replica's registry as Served reads it: a service absent from it is not served (disabled, refused,
// not loaded yet).
type servedMap struct {
	mu sync.Mutex
	m  map[string]ServedService
}

func (s *servedMap) set(name string, svc *ServedService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc == nil {
		delete(s.m, name)
		return
	}
	s.m[name] = *svc
}

func (s *servedMap) served(_ context.Context, _ uuid.UUID, name string) (ServedService, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[name]
	return v, ok
}

func allowOf(t *testing.T, entries ...string) auth.AllowList {
	t.Helper()
	l, err := auth.ParseAllowList(entries)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// MCP-1: a hire task is the reader's on /mcp only while this replica serves its service to the reader, as the A2A route
// answers GetTask, CancelTask and a follow-up 404 "unknown service" otherwise. A key that shares the buyer of the key
// that hired, but that the service's allow: does not admit, gets -32602 from tasks/get, tasks/result, tasks/cancel
// and tasks/update, sees nothing in tasks/list, and its reply is "unknown task": nothing is cancelled or sent. So does
// the hiring key once its allow: entry is removed, and once the service is disabled; the task is left as it was.
func TestHireTaskSurfacesFollowTheRegistry(t *testing.T) {
	e := newModernEnv(t)
	hirer := &followRecorder{pool: e.pool}
	reg := &servedMap{m: map[string]ServedService{"hello": {Kind: config.KindA2A, Allow: allowOf(t, "key:kk_buyer")}}}
	tasks, err := NewHireTasks(HireTasksConfig{Log: store.NewLog(e.pool), Waker: noWake{}, Lifecycle: e.cancel, Hirer: hirer,
		Served: reg.served, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{deps: Deps{PublicURL: "https://gate.example", Hirer: hirer, Tasks: tasks, Log: store.NewLog(e.pool),
		Waker: noWake{}, Served: reg.served, PlainWait: time.Second}, log: slog.New(slog.DiscardHandler)}
	other := e.buyer
	other.ID = "key:kk_other" // the same buyer, another key
	asking := e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil), 2, true)
	ctx := context.Background()
	key := store.GateRequestID(asking, 3)

	refused := func(what string, p auth.Principal) {
		t.Helper()
		rd := Reader{Principal: p, Era: mcpwire.EraLegacy, FormElicitation: true}
		if _, rpcErr, err := tasks.Get(ctx, rd, asking); err != nil || rpcErr != errTaskNotFound {
			t.Errorf("%s: tasks/get %v %v", what, rpcErr, err)
		}
		if res, _, err := tasks.List(ctx, rd, json.RawMessage(`{}`)); err != nil || strings.Contains(string(res), asking.String()) {
			t.Errorf("%s: tasks/list %s %v", what, res, err)
		}
		if _, rpcErr, err := tasks.Cancel(ctx, rd, asking); err != nil || rpcErr != errTaskNotFound {
			t.Errorf("%s: tasks/cancel %v %v", what, rpcErr, err)
		}
		mrd := Reader{Principal: p, Era: mcpwire.EraModern, FormElicitation: true}
		if _, rpcErr, err := tasks.Update(ctx, mrd, json.RawMessage(`{"taskId":"`+asking.String()+`","inputResponses":{"`+key+`":`+accept("x")+`}}`)); err != nil || rpcErr != errTaskNotFound {
			t.Errorf("%s: tasks/update %v %v", what, rpcErr, err)
		}
		if tasks.Respond(ctx, rd, &mcpwire.Message{ID: json.RawMessage(`"` + key + `"`), Result: json.RawMessage(accept("x"))}) {
			t.Errorf("%s: a response to the question was taken", what)
		}
		w := httptest.NewRecorder()
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second) // a task served to the reader would block until it ends
		tasks.Result(rctx, w, "", rd, &mcpwire.Message{ID: json.RawMessage(`1`), Method: "tasks/result",
			Params: json.RawMessage(`{"taskId":"` + asking.String() + `"}`)})
		cancel()
		if a := answerOf(t, w); a.Error == nil || a.Error.Code != mcpwire.CodeInvalidParams {
			t.Errorf("%s: tasks/result %s", what, a.body)
		}
		rq, rw := legacyCall(t, p, ToolReply, map[string]any{"taskId": asking.String(), "message": "x"})
		h.toolsCall(rq)
		if r, a := toolAnswer(t, rw); !r.IsError || len(r.Content) != 1 || r.Content[0].Text != "unknown task" {
			t.Errorf("%s: reply %s", what, a.body)
		}
		if got := hirer.replies(); len(got) != 0 {
			t.Fatalf("%s: follow-ups sent %v", what, got)
		}
		if got := e.cancel.calls(); len(got) != 0 {
			t.Fatalf("%s: cancels sent %v", what, got)
		}
		if st := e.state(asking); st != "input-required" {
			t.Fatalf("%s: the task is %s", what, st)
		}
	}
	// The control: the hiring key reads it.
	rd := Reader{Principal: e.buyer, Era: mcpwire.EraLegacy}
	if res, rpcErr, err := tasks.Get(ctx, rd, asking); err != nil || rpcErr != nil || !strings.Contains(string(res), asking.String()) {
		t.Fatalf("the hiring key's tasks/get: %s %v %v", res, rpcErr, err)
	}
	refused("a key of the same buyer that allow: does not admit", other)
	reg.set("hello", &ServedService{Kind: config.KindA2A, Allow: allowOf(t, "key:kk_someone")})
	refused("the hiring key once its allow: entry is removed", e.buyer)
	reg.set("hello", nil)
	refused("the hiring key once the service is disabled", e.buyer)
	reg.set("hello", &ServedService{Kind: config.KindMCP, Allow: allowOf(t, "key:kk_buyer")})
	refused("a service of another kind under that name", e.buyer)
}

// MCP-3, MCP-4: search, describe and hire see exactly what the replica's registry serves. A row the registry does not
// serve (one it refused, or has not loaded yet) is left out of search and is an unknown service to describe and hire;
// the registry's allow: decides. A row whose allow: does not parse (a stored oidc:sub:*, refused since D301) never
// fails a search, whoever asks, nor describe or hire: it is left out, as the registry leaves it out.
func TestCatalogServesWhatTheRegistryServes(t *testing.T) {
	e := newCatalogEnv(t)
	ctx := context.Background()
	org := []string{"org:acme"}
	for _, n := range []string{"agent", "bad", "notloaded", "zzz"} {
		e.service(e.acme, n, "a2a", directA2A, org, false, "", `{"name":"`+n+`","description":"d"}`, false)
	}
	e.exec(`update kontra.services set allow = '{oidc:sub:*}' where name = 'bad'`)
	p := keyPrincipal(e.acme, "acme", "kk_acme_1")
	names := func(c *Catalog) string {
		t.Helper()
		res, err := c.Search(ctx, p, SearchArgs{})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		var out []string
		for _, s := range res.Services {
			out = append(out, s.Name)
		}
		return strings.Join(out, ",")
	}

	// Without the registry (the services table alone): the unreadable row is left out, and the others are served.
	plain := NewCatalog(CatalogConfig{DB: e.pool, Multi: true, Logger: slog.New(slog.DiscardHandler)})
	if got := names(plain); got != "agent,notloaded,zzz" {
		t.Errorf("search past an unreadable row: %q", got)
	}
	for _, c := range []struct{ name string }{{"bad"}, {"agent"}} {
		ref := Ref{Tenant: "acme", Name: c.name}
		svc, err := plain.Hireable(ctx, p, ref)
		if err != nil || (svc == nil) != (c.name == "bad") {
			t.Errorf("hire of %s: %v %v", c.name, svc, err)
		}
		if _, err := plain.Describe(ctx, p, ref); (err == ErrUnknownService) != (c.name == "bad") || (err != nil && err != ErrUnknownService) {
			t.Errorf("describe of %s: %v", c.name, err)
		}
	}

	// With the registry: only what it serves, admitted by its allow:.
	reg := &servedMap{m: map[string]ServedService{
		"agent": {Kind: config.KindA2A, Allow: allowOf(t, "org:acme")},
		"zzz":   {Kind: config.KindA2A, Allow: allowOf(t, "key:kk_nobody")}, // the registry's allow: has moved on
	}}
	c := NewCatalog(CatalogConfig{DB: e.pool, Multi: true, Served: reg.served, Logger: slog.New(slog.DiscardHandler)})
	if got := names(c); got != "agent" {
		t.Errorf("search with the registry: %q, want agent", got)
	}
	for name, want := range map[string]bool{"agent": true, "bad": false, "notloaded": false, "zzz": false} {
		ref := Ref{Tenant: "acme", Name: name}
		svc, err := c.Hireable(ctx, p, ref)
		if err != nil || (svc != nil) != want {
			t.Errorf("hire of %s: %v %v, want hireable %v", name, svc, err, want)
		}
		if _, err := c.Describe(ctx, p, ref); (err == nil) != want || (err != nil && err != ErrUnknownService) {
			t.Errorf("describe of %s: %v", name, err)
		}
	}
}

// merge-1: a legacy session of /mcp is in use while its GET stream is open: the replica keeps its last_seen_at fresh,
// so the idle sweep (SweepGateIdle) leaves it, and the client's next request is served; once the stream closes, the
// session idles out as before.
func TestGateSessionInUseOutlivesTheIdleSweep(t *testing.T) {
	saved := mcprelay.IdleWrite
	mcprelay.IdleWrite = 200 * time.Millisecond
	t.Cleanup(func() { mcprelay.IdleWrite = saved })
	e := newModernEnv(t)
	sessions := mcprelay.NewSessions(e.pool)
	el := NewElicitations(store.NewLog(e.pool), noWake{}, slog.New(slog.DiscardHandler))
	h := &Handler{deps: Deps{PublicURL: "https://gate.example", Sessions: sessions, Elicitations: el, Tasks: e.tasks,
		Log: store.NewLog(e.pool), Waker: noWake{}, now: time.Now}, log: slog.New(slog.DiscardHandler)}
	sid, _, err := mcprelay.MintID()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := sessions.OpenGate(ctx, sid, mcprelay.Session{TenantID: e.team, BuyerID: e.buyer.BuyerID, Principal: e.buyer.ID,
		Via: string(e.buyer.Via), ProtocolVersion: mcpwire.V20251125}); err != nil {
		t.Fatal(err)
	}
	serve := func(ctx context.Context, method, body string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		r := httptest.NewRequestWithContext(ctx, method, Path, rdr)
		r.Header.Set(mcpwire.HeaderSessionID, sid)
		w := httptest.NewRecorder()
		rq := &request{w: w, r: r, p: e.buyer, start: time.Now()}
		if body != "" {
			m, err := mcpwire.Parse([]byte(body), mcpwire.FromClient)
			if err != nil {
				t.Fatal(err)
			}
			rq.msg, rq.body = m, []byte(body)
		}
		h.session(rq)
		return w
	}
	sctx, stop := context.WithCancel(ctx)
	streamed := make(chan struct{})
	go func() {
		defer close(streamed)
		serve(sctx, http.MethodGet, "")
	}()
	time.Sleep(1500 * time.Millisecond)
	// The sweep, with session_idle shortened to 600 ms: the session has had no request for 1.5 s, but its stream is open.
	if n, err := sessions.SweepGateIdle(ctx, 600*time.Millisecond, time.Now()); err != nil || n != 0 {
		t.Fatalf("the idle sweep closed %d sessions while the GET stream is open (%v)", n, err)
	}
	if w := serve(ctx, http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); w.Code != http.StatusOK {
		t.Fatalf("the next request: %d %s", w.Code, w.Body)
	}
	stop()
	<-streamed
	time.Sleep(time.Second)
	if n, err := sessions.SweepGateIdle(ctx, 600*time.Millisecond, time.Now()); err != nil || n != 1 {
		t.Fatalf("the idle sweep after the stream closed: %d %v, want the session closed", n, err)
	}
}

// merge-3: the response stream of /mcp's tasks/result carries no event ids, as /mcp's GET stream and a plain hire's do
// (relay.md §12.3): its elicitation/create and its outcome are plain events, since /mcp resumes no stream.
func TestGateTasksResultStreamHasNoEventIDs(t *testing.T) {
	e := newModernEnv(t)
	e.tasks.reread = 50 * time.Millisecond
	id := e.task(e.buyer.BuyerID, store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil), 2, true)
	rd := Reader{Principal: e.buyer, Era: mcpwire.EraLegacy, FormElicitation: true}
	go func() {
		time.Sleep(300 * time.Millisecond)
		complete(t, e.pool, id, "booked")
	}()
	w := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.tasks.Result(ctx, w, "sid", rd, &mcpwire.Message{ID: json.RawMessage(`1`), Method: "tasks/result",
		Params: json.RawMessage(`{"taskId":"` + id.String() + `"}`)})
	var events, ids int
	sc := bufio.NewScanner(bytes.NewReader(w.Body.Bytes()))
	for sc.Scan() {
		switch line := sc.Text(); {
		case strings.HasPrefix(line, "id:"):
			ids++
		case strings.HasPrefix(line, "data:"):
			events++
		}
	}
	if events != 2 || ids != 0 || !strings.Contains(w.Body.String(), `"method":"elicitation/create"`) || !strings.Contains(w.Body.String(), "booked") {
		t.Fatalf("tasks/result stream: %d events, %d event ids:\n%s", events, ids, w.Body)
	}
}

// presignStore is a bucket whose presigned GETs name the object's key.
type presignStore struct{ blob.Store }

func (presignStore) Presign(_ context.Context, k blob.Key, _ time.Duration) (string, error) {
	return "https://bucket.example/" + string(k.Ref) + "?sig=1", nil
}

// merge-2: /mcp renders a hire task's bucketed parts as presigned GETs for every reader, a request that arrived on a
// node's hiring stream (blob.WithKeepRefs) included: the node relays an MCP answer as it is, so a kontra-blob: ref left
// in it would be a resource_link no client can fetch.
func TestGateMCPRendersBlobsThroughANode(t *testing.T) {
	e := newModernEnv(t)
	ref, err := blob.NewRef()
	if err != nil {
		t.Fatal(err)
	}
	task := taskWith(a2a.TaskStateCompleted, &a2a.Message{ID: "m", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{
		a2a.NewTextPart("done"), &a2a.Part{Content: a2a.URL(ref.URL()), Filename: "big.bin", MediaType: "application/octet-stream"}}}, nil)
	id := e.task(e.buyer.BuyerID, store.StateCompleted, task, 2, true)
	if _, err := e.pool.Exec(context.Background(), `insert into kontra.blobs (ref, tenant_id, task_id, size) values ($1, $2, $3, 300000)`,
		string(ref), e.team, id); err != nil {
		t.Fatal(err)
	}
	render := blob.NewRenderer(presignStore{}, e.pool, slog.New(slog.DiscardHandler))
	tasks, err := NewHireTasks(HireTasksConfig{Log: store.NewLog(e.pool), Waker: noWake{}, Lifecycle: e.cancel, Hirer: e.hirer,
		Render: render, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	node := blob.WithKeepRefs(context.Background()) // a request on a hiring stream
	check := func(what, body string) {
		t.Helper()
		if strings.Contains(body, blob.URIScheme) || !strings.Contains(body, "https://bucket.example/") {
			t.Errorf("%s through a node: %s", what, body)
		}
	}
	w := httptest.NewRecorder()
	tasks.Result(node, w, "", Reader{Principal: e.buyer, Era: mcpwire.EraLegacy}, &mcpwire.Message{ID: json.RawMessage(`1`),
		Method: "tasks/result", Params: json.RawMessage(`{"taskId":"` + id.String() + `"}`)})
	check("tasks/result", w.Body.String())
	res, _, err := tasks.Get(node, Reader{Principal: e.buyer, Era: mcpwire.EraModern}, id)
	if err != nil {
		t.Fatal(err)
	}
	check("a modern tasks/get", string(res))
	h := &Handler{deps: Deps{PublicURL: "https://gate.example", Log: store.NewLog(e.pool), Render: render}, log: slog.New(slog.DiscardHandler)}
	row, err := store.NewLog(e.pool).Load(node, id)
	if err != nil {
		t.Fatal(err)
	}
	check("a plain hire's answer", string(h.plainAnswer(node, row, e.buyer)))
}
