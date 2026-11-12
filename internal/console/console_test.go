package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/blob/blobtest"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tenant"
)

// helloCard is a served card declaring A2UI v0.9.1 and AP2, with one skill.
const helloCard = `{"name":"Hello","description":"Says hello.","url":"http://gate/agents/hello",
	"capabilities":{"extensions":[{"uri":"https://a2ui.org/a2a-extension/a2ui/v0.9.1"},
	{"uri":"https://github.com/google-agentic-commerce/ap2/v1","required":true}]},
	"defaultInputModes":["text/plain"],"skills":[{"id":"hi","name":"Say hi","examples":["hi there",3]}]}`

// env is a migrated database with the team tenant and service hello, the console over it at srv, and a fake public
// listener (gate) the console's prompting reaches.
type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	log    *store.Log
	team   uuid.UUID
	buyer  uuid.UUID
	c      *Console
	srv    *httptest.Server
	gate   *httptest.Server
	mu     sync.Mutex
	gotReq []*http.Request
	gotMsg []map[string]any
	answer func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

func newEnv(t *testing.T, blobs blob.Store) *env {
	t.Helper()
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	e := &env{t: t, pool: pool, log: store.NewLog(pool)}
	ctx := context.Background()
	var err error
	if e.team, err = tenant.EnsureTeam(ctx, pool); err != nil {
		t.Fatal(err)
	}
	e.buyer, _ = ids.AnonymousBuyer(uuid.Nil)
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, allow, source, card, card_fetched_at)
		values ($1, 'hello', 'a2a', '{"tunnel": {}}', '{anonymous}', 'db', $2::json, now()),
		       ($1, 'tools', 'mcp', '{"tunnel": {}}', '{anonymous}', 'db', null, null)`, e.team, helloCard)
	e.gate = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		e.gotReq = append(e.gotReq, r)
		e.gotMsg = append(e.gotMsg, body)
		answer := e.answer
		e.mu.Unlock()
		if answer != nil {
			answer(w, r, body)
			return
		}
		http.Error(w, "no answer", http.StatusInternalServerError)
	}))
	t.Cleanup(e.gate.Close)
	listener := pg.NewListener(pool.Config().ConnConfig)
	lctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = listener.Run(lctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	cfg := &config.Config{Profile: config.ProfileLocal, PublicURL: e.gate.URL,
		Traffic: config.Traffic{Retention: config.Retention(24 * time.Hour)}}
	on := true
	cfg.Traffic.Record = &on
	e.c = New(Deps{Config: cfg, DB: pool, Log: e.log, Waker: listener, Blobs: blobs, Dropped: func() int64 { return 3 }})
	routes := edge.NewRouter()
	e.c.Register(routes)
	e.srv = httptest.NewServer(routes)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// task creates an A2A task of hello in state with the first message text, in context ctxID; console marks it the
// console's.
func (e *env) task(state store.State, text, ctxID string, console bool) uuid.UUID {
	e.t.Helper()
	return e.taskWith(state, text, ctxID, console, nil)
}

// streamed is the gate's answer to the console's message:stream (D362): it records the message as the gate's Write
// does (its first_message_id the console's messageId), and writes the stream's headers.
func (e *env) streamed(w http.ResponseWriter, body map[string]any, state store.State, ctxID string) uuid.UUID {
	e.t.Helper()
	msg, _ := body["message"].(map[string]any)
	id := e.task(state, "hi", ctxID, false)
	e.exec(`update kontra.tasks set first_message_id = $2, first_request = $3 where id = $1`, id, msg["messageId"], make([]byte, 32))
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return id
}

// taskWith is task with the first event's origin (D357).
func (e *env) taskWith(state store.State, text, ctxID string, console bool, origin *store.Origin) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), ctxID
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: ctxID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{msg}}
	svc := store.ServiceRef{TenantID: e.team, Tenant: "default", Name: "hello", Kind: "a2a"}
	nt := store.NewTask{ID: id, Service: svc, Protocol: store.ProtocolA2A, ContextID: ctxID, State: store.StateSubmitted, Snapshot: snap,
		Owner: store.Owner{BuyerID: e.buyer, Principal: "anonymous", Via: store.ViaAnonymous}, Origin: origin}
	if _, err := e.log.CreateTask(context.Background(), nil, nt); err != nil {
		e.t.Fatal(err)
	}
	if state != store.StateSubmitted {
		terminal := map[store.State]bool{store.StateCompleted: true, store.StateFailed: true, store.StateCanceled: true, store.StateRejected: true}[state]
		if terminal {
			e.exec(`update kontra.tasks set state = $2, terminal_at = now() where id = $1`, id, string(state))
		} else {
			e.exec(`update kontra.tasks set state = $2 where id = $1`, id, string(state))
		}
	}
	if console {
		e.exec(`insert into kontra.console_tasks (task_id) values ($1)`, id)
	}
	return id
}

// event appends one event row of kind with payload to the task, as the log does (version, NOTIFY).
func (e *env) event(id uuid.UUID, kind string, payload string) int64 {
	e.t.Helper()
	var seq int64
	err := e.pool.QueryRow(context.Background(), `with v as (update kontra.tasks set version = version + 1 where id = $1 returning version)
		insert into kontra.task_events (task_id, seq, kind, payload) select $1, v.version, $2, $3::json from v returning seq`,
		id, kind, payload).Scan(&seq)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`select pg_notify('kontra_task_events', $1)`, fmt.Sprintf("%s:%d", id, seq))
	return seq
}

func (e *env) do(method, path string, body string, hdr ...string) *http.Response {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func (e *env) json(method, path, body string, want int, hdr ...string) map[string]any {
	e.t.Helper()
	resp := e.do(method, path, body, hdr...)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, b)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
	}
	return out
}

func items(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, x := range m["items"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

// ---------------------------------------------------------------- 4.1, 4.2

// The console's routes are the closed router's empty 404 unless registered (outside the profile nothing registers).
func TestNotMountedUnregistered(t *testing.T) {
	routes := edge.NewRouter()
	routes.Handle("GET /healthz", edge.Healthz(func() bool { return true }))
	srv := httptest.NewServer(edge.Middleware(edge.MiddlewareConfig{}, routes))
	defer srv.Close()
	for _, p := range []string{"/", "/v1/tasks", "/v1/info", "/js/main.js"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || len(b) != 0 {
			t.Errorf("GET %s = %d %q, want the closed route's empty 404", p, resp.StatusCode, b)
		}
	}
}

// Hardening: no CORS header; a foreign Origin (a cross-site fetch, a cross-site form post) and a cross-site
// Sec-Fetch-Site are 403 before any handler; a state-changing request that is not application/json is 403; the page
// carries the security headers.
func TestHardening(t *testing.T) {
	e := newEnv(t, nil)
	for _, c := range []struct {
		name, method, path, body string
		hdr                      []string
		want                     int
	}{
		{"same origin read", "GET", "/v1/info", "", []string{"Origin", e.srv.URL}, 200},
		{"no origin read", "GET", "/v1/info", "", nil, 200},
		{"cross-site fetch", "GET", "/v1/tasks", "", []string{"Origin", "http://attacker.example"}, 403},
		{"null origin", "GET", "/v1/tasks", "", []string{"Origin", "null"}, 403},
		{"sec-fetch cross-site", "GET", "/v1/tasks", "", []string{"Sec-Fetch-Site", "cross-site"}, 403},
		{"cross-site form post", "POST", "/v1/tasks", "service=hello&text=hi",
			[]string{"Origin", "http://attacker.example", "Content-Type", "application/x-www-form-urlencoded"}, 403},
		{"same-origin text/plain post", "POST", "/v1/tasks", `{"service":"hello","text":"hi"}`, []string{"Content-Type", "text/plain"}, 403},
		{"same-origin form post", "POST", "/v1/tasks", "a=b", []string{"Content-Type", "application/x-www-form-urlencoded"}, 403},
	} {
		resp := e.do(c.method, c.path, c.body, c.hdr...)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
		for k := range resp.Header {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Errorf("%s: CORS header %s", c.name, k)
			}
		}
	}
	if n := e.count(`select count(*) from kontra.console_tasks`); n != 0 || len(e.gotReq) != 0 {
		t.Fatal("a refused request reached the handler")
	}
	// A preflight is never answered.
	resp := e.do("OPTIONS", "/v1/tasks", "", "Origin", "http://attacker.example", "Access-Control-Request-Method", "POST")
	resp.Body.Close()
	if resp.StatusCode < 400 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight = %d %v", resp.StatusCode, resp.Header)
	}
	page := e.do("GET", "/", "")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != 200 || !bytes.Contains(body, []byte(`js/main.js`)) {
		t.Fatalf("GET / = %d", page.StatusCode)
	}
	for k, v := range map[string]string{"Content-Security-Policy": CSP, "X-Content-Type-Options": "nosniff",
		"Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
		if got := page.Header.Get(k); got != v {
			t.Errorf("page %s = %q, want %q", k, got, v)
		}
	}
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "object-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(CSP, want) {
			t.Errorf("CSP lacks %s", want)
		}
	}
	js := e.do("GET", "/js/main.js", "")
	js.Body.Close()
	if js.StatusCode != 200 || js.Header.Get("Content-Security-Policy") != CSP {
		t.Errorf("GET /js/main.js = %d", js.StatusCode)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------- 4.3

func TestInfo(t *testing.T) {
	e := newEnv(t, nil)
	e.exec(`update kontra.services set retention_s = 0 where name = 'hello'`)
	got := e.json("GET", "/v1/info", "", 200)
	ret := got["retention"].(map[string]any)
	if got["profile"] != "local" || ret["traffic"] != float64(86400) || ret["services"].(map[string]any)["hello"] != float64(0) {
		t.Fatalf("info %v", got)
	}
	if tr := got["traffic"].(map[string]any); tr["dropped"] != float64(3) || tr["recording"] != true {
		t.Fatalf("info traffic %v", tr)
	}
	feats := fmt.Sprint(got["features"])
	for _, f := range []string{"tasks", "conversations", "traffic", "calls", "prompt"} {
		if !strings.Contains(feats, f) {
			t.Errorf("features %s lack %s", feats, f)
		}
	}
}

// ---------------------------------------------------------------- 4.4, 4.5, 4.6

// Every caller's tasks, newest first, with the console's and another client's alike; filters and pages.
func TestTasksList(t *testing.T) {
	e := newEnv(t, nil)
	client := e.task(store.StateWorking, "from a client", "c-client", false)
	mine := e.task(store.StateInputRequired, "from the console", "c-console", true)
	done := e.task(store.StateCompleted, "an old one", "c-console", true)
	got := items(e.json("GET", "/v1/tasks", "", 200))
	if len(got) != 3 || got[0]["task_id"] != done.String() || got[1]["task_id"] != mine.String() || got[2]["task_id"] != client.String() {
		t.Fatalf("tasks %v", got)
	}
	if got[1]["via"] != "console" || got[2]["via"] != "anonymous" || got[2]["summary"] != "from a client" || got[2]["caller"] != "anonymous" {
		t.Fatalf("task rows %v / %v", got[1], got[2])
	}
	if s := items(e.json("GET", "/v1/tasks?state=input-required", "", 200)); len(s) != 1 || s[0]["task_id"] != mine.String() {
		t.Fatalf("state filter %v", s)
	}
	if s := items(e.json("GET", "/v1/tasks?kind=mcp", "", 200)); len(s) != 0 {
		t.Fatalf("kind filter %v", s)
	}
	if s := items(e.json("GET", "/v1/tasks?service=nope", "", 200)); len(s) != 0 {
		t.Fatalf("service filter %v", s)
	}
	page := e.json("GET", "/v1/tasks?limit=2", "", 200)
	if len(items(page)) != 2 || page["next"] == nil {
		t.Fatalf("page 1 %v", page)
	}
	page2 := e.json("GET", "/v1/tasks?limit=2&before="+page["next"].(string), "", 200)
	if p := items(page2); len(p) != 1 || p[0]["task_id"] != client.String() || page2["next"] != nil {
		t.Fatalf("page 2 %v", page2)
	}
	e.json("GET", "/v1/tasks?limit=101", "", 422)
}

// Two tasks in one context are one conversation; its tasks oldest first.
func TestConversations(t *testing.T) {
	e := newEnv(t, nil)
	first := e.task(store.StateCompleted, "one", "ctx-1", true)
	second := e.task(store.StateInputRequired, "two", "ctx-1", false)
	e.task(store.StateWorking, "other", "ctx-2", false)
	got := items(e.json("GET", "/v1/conversations", "", 200))
	if len(got) != 2 {
		t.Fatalf("conversations %v", got)
	}
	var c1 map[string]any
	for _, c := range got {
		if c["context_id"] == "ctx-1" {
			c1 = c
		}
	}
	if c1 == nil || c1["tasks"] != float64(2) || c1["state"] != "input-required" || c1["caller"] != e.buyer.String() {
		t.Fatalf("ctx-1 %v", c1)
	}
	one := e.json("GET", "/v1/conversations/hello/"+e.buyer.String()+"/ctx-1", "", 200)
	ts := one["tasks"].([]any)
	if len(ts) != 2 || ts[0].(map[string]any)["task_id"] != first.String() || ts[1].(map[string]any)["task_id"] != second.String() {
		t.Fatalf("conversation tasks %v", ts)
	}
	e.json("GET", "/v1/conversations/hello/"+e.buyer.String()+"/nope", "", 404)
	e.json("GET", "/v1/conversations/hello/not-a-uuid/ctx-1", "", 404)
	if p := e.json("GET", "/v1/conversations?limit=1", "", 200); p["next"] == nil ||
		len(items(e.json("GET", "/v1/conversations?limit=1&before="+p["next"].(string), "", 200))) != 1 {
		t.Fatalf("conversation pages %v", p)
	}
}

// The actions of a task view: only the console's own tasks, reply only at input-required, cancel until it ends,
// continue once it has; another client's task is watch-only; an unknown one 404.
func TestTaskActions(t *testing.T) {
	e := newEnv(t, nil)
	for _, c := range []struct {
		state   store.State
		console bool
		want    string
	}{
		{store.StateInputRequired, true, "[reply cancel]"},
		{store.StateWorking, true, "[cancel]"},
		{store.StateSubmitted, true, "[cancel]"},
		{store.StateCompleted, true, "[continue]"},
		{store.StateFailed, true, "[continue]"},
		{store.StateInputRequired, false, "[]"},
		{store.StateCompleted, false, "[]"},
	} {
		id := e.task(c.state, "hi", "ctx-"+string(c.state), c.console)
		got := e.json("GET", "/v1/tasks/"+id.String(), "", 200)
		if a := fmt.Sprint(got["actions"]); a != c.want {
			t.Errorf("%s console=%v: actions %s, want %s", c.state, c.console, a, c.want)
		}
		if got["snapshot"] == nil || got["context_id"] != "ctx-"+string(c.state) {
			t.Errorf("task view %v", got)
		}
	}
	e.json("GET", "/v1/tasks/"+uuid.NewString(), "", 404)
	e.json("GET", "/v1/tasks/not-an-id", "", 404)
	e.exec(`update kontra.services set sealed = 'required' where name = 'hello'`)
	id := e.task(store.StateInputRequired, "hi", "ctx-sealed", true)
	if got := e.json("GET", "/v1/tasks/"+id.String(), "", 200); fmt.Sprint(got["actions"]) != "[]" || got["summary"] != nil || got["content"] != false {
		t.Errorf("sealed task %v", got)
	}
}

// ---------------------------------------------------------------- 4.7

type sseEvent struct {
	id, event, data string
}

// sseReader reads one stream's events, in one goroutine.
type sseReader struct{ ch chan sseEvent }

func newSSE(body io.Reader) *sseReader {
	r := &sseReader{ch: make(chan sseEvent, 64)}
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.data != "" {
					r.ch <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				cur.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				cur.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				cur.data += line[6:]
			}
		}
		close(r.ch)
	}()
	return r
}

func (r *sseReader) next(t *testing.T, n int, timeout time.Duration) []sseEvent {
	t.Helper()
	var got []sseEvent
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case ev, ok := <-r.ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("read %d of %d events: %v", len(got), n, got)
		}
	}
	return got
}

// readSSE reads n events of a stream read nowhere else.
func readSSE(t *testing.T, body io.Reader, n int, timeout time.Duration) []sseEvent {
	t.Helper()
	return newSSE(body).next(t, n, timeout)
}

// A live artifact event reaches an open stream; a reconnect from seq 7 gets 8 on without a gap or a duplicate; an
// ended task's stream ends with an end event.
func TestTaskEvents(t *testing.T) {
	e := newEnv(t, nil)
	id := e.task(store.StateWorking, "hi", "ctx-e", true)
	resp := e.do("GET", "/v1/tasks/"+id.String()+"/events", "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	live := newSSE(resp.Body)
	first := live.next(t, 1, 5*time.Second)
	if first[0].id != "1" || first[0].event != "task" {
		t.Fatalf("first event %v", first)
	}
	art := `{"artifactUpdate":{"taskId":"` + id.String() + `","contextId":"ctx-e","artifact":{"artifactId":"a1","parts":[{"text":"live"}]}}}`
	start := time.Now()
	e.event(id, "artifact", art)
	got := live.next(t, 1, 5*time.Second)
	var d map[string]any
	if err := json.Unmarshal([]byte(got[0].data), &d); err != nil || got[0].id != "2" || got[0].event != "artifact" || d["seq"] != float64(2) ||
		!strings.Contains(got[0].data, `"live"`) {
		t.Fatalf("live event %v (%v)", got, err)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Errorf("the live event took %s (the NOTIFY wake should beat the poll)", time.Since(start))
	}
	resp.Body.Close()

	for range 6 {
		e.event(id, "status", `{"statusUpdate":{"taskId":"`+id.String()+`","contextId":"ctx-e","status":{"state":"TASK_STATE_WORKING"}}}`)
	}
	// Last-Event-ID 7 then two more: 8, 9, and nothing again.
	re := e.do("GET", "/v1/tasks/"+id.String()+"/events", "", "Last-Event-ID", "7")
	defer re.Body.Close()
	rs := newSSE(re.Body)
	again := rs.next(t, 1, 5*time.Second)
	if again[0].id != "8" {
		t.Fatalf("reconnect from 7 began at %v", again)
	}
	e.exec(`update kontra.tasks set state = 'completed', terminal_at = now() where id = $1`, id)
	e.event(id, "status", `{"statusUpdate":{"taskId":"`+id.String()+`","contextId":"ctx-e","status":{"state":"TASK_STATE_COMPLETED"}}}`)
	tail := rs.next(t, 2, 5*time.Second)
	if tail[0].id != "9" || tail[1].event != "end" || !strings.Contains(tail[1].data, `"completed"`) {
		t.Fatalf("tail %v", tail)
	}
	if more := rs.next(t, 1, 5*time.Second); len(more) != 0 {
		t.Fatalf("events after end: %v", more)
	}
	// An ended task whose every event the reader has: end at once.
	ended := e.do("GET", "/v1/tasks/"+id.String()+"/events?after=9", "")
	defer ended.Body.Close()
	if ev := readSSE(t, ended.Body, 1, 5*time.Second); ev[0].event != "end" {
		t.Fatalf("ended stream %v", ev)
	}
	if r := e.do("GET", "/v1/tasks/"+uuid.NewString()+"/events", ""); r.StatusCode != 404 {
		t.Fatalf("unknown task's events = %d", r.StatusCode)
	}
}

// ---------------------------------------------------------------- 4.8

// A part stored apart is served for its own task only.
func TestTaskFile(t *testing.T) {
	bucket := blobtest.New(t, "")
	e := newEnv(t, bucket.Store)
	id := e.task(store.StateCompleted, "hi", "ctx-f", false)
	other := e.task(store.StateCompleted, "hi", "ctx-g", false)
	ref, _ := blob.NewRef()
	big := bytes.Repeat([]byte("k"), 2<<20)
	if err := bucket.Store.Put(context.Background(), blob.Key{Tenant: e.team, Task: id, Ref: ref}, bytes.NewReader(big), int64(len(big))); err != nil {
		t.Fatal(err)
	}
	e.exec(`insert into kontra.blobs (ref, tenant_id, task_id, size) values ($1, $2, $3, $4)`, string(ref), e.team, id, len(big))
	resp := e.do("GET", "/v1/tasks/"+id.String()+"/files/"+string(ref), "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(b, big) || resp.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("file = %d, %d bytes", resp.StatusCode, len(b))
	}
	for _, p := range []string{"/v1/tasks/" + other.String() + "/files/" + string(ref), "/v1/tasks/" + id.String() + "/files/nope"} {
		if r := e.do("GET", p, ""); r.StatusCode != 404 {
			t.Errorf("GET %s = %d, want 404", p, r.StatusCode)
		}
	}
}

// ---------------------------------------------------------------- 4.9

func TestServices(t *testing.T) {
	e := newEnv(t, nil)
	got := items(e.json("GET", "/v1/services", "", 200))
	if len(got) != 2 || got[0]["name"] != "hello" || got[0]["reachable"] != false || got[0]["sessions"] != float64(0) || got[0]["retention"] != float64(604800) {
		t.Fatalf("services %v", got)
	}
	ag, _ := got[0]["agent"].(map[string]any)
	if ag == nil || ag["name"] != "Hello" || ag["description"] != "Says hello." || ag["version"] != nil || got[1]["agent"] != nil {
		t.Fatalf("agent summary %v / %v", got[0]["agent"], got[1]["agent"])
	}
	if sk := ag["skills"].([]any)[0].(map[string]any); sk["name"] != "Say hi" || fmt.Sprint(sk["examples"]) != "[hi there]" || fmt.Sprint(sk["tags"]) != "[]" {
		t.Fatalf("summary skills %v", sk)
	}
	e.exec(`insert into kontra.gate_replicas (replica_id, version) values ('r1', 'test')`)
	e.exec(`insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ('k1_aaaaaaaaaaaaaaaaaaaaaaaaaa', $1, decode(repeat('01', 32), 'hex'), 'laptop')`, e.team)
	e.exec(`insert into kontra.tunnels (session_id, key_id, tenant_id, replica_id, services, cli_version)
		values ($1, 'k1_aaaaaaaaaaaaaaaaaaaaaaaaaa', $2, 'r1', '{hello}', 'kontra/test')`, uuid.New(), e.team)
	one := e.json("GET", "/v1/services/hello", "", 200)
	if one["reachable"] != true || one["sessions"] != float64(1) || one["card"] == nil {
		t.Fatalf("service %v", one)
	}
	e.json("GET", "/v1/services/nope", "", 404)
	tun := items(e.json("GET", "/v1/tunnels", "", 200))
	if len(tun) != 1 || tun[0]["node_name"] != "laptop" || fmt.Sprint(tun[0]["services"]) != "[hello]" {
		t.Fatalf("tunnels %v", tun)
	}
	form := e.json("GET", "/v1/services/hello/form", "", 200)
	if form["name"] != "Hello" || form["a2ui"] != ExtA2UI091 || form["ap2"] != true || fmt.Sprint(form["unsupported_extensions"]) != "[]" {
		t.Fatalf("form %v", form)
	}
	if sk := form["skills"].([]any)[0].(map[string]any); sk["name"] != "Say hi" || fmt.Sprint(sk["examples"]) != "[hi there]" {
		t.Fatalf("skills %v", sk)
	}
	e.json("GET", "/v1/services/tools/form", "", 409)
}

// ---------------------------------------------------------------- 4.10

func TestExchangesAndCalls(t *testing.T) {
	e := newEnv(t, nil)
	task := e.task(store.StateWorking, "hi", "ctx-x", false)
	for i, x := range []struct {
		method string
		status int
		code   any
		reason any
		task   any
	}{
		{"SendMessage", 200, nil, nil, task},
		{"GetTask", 200, nil, nil, task},
		{"SendMessage", 200, -32009, "version_unsupported", nil},
		{"unknown", 413, nil, "body_too_large", nil},
	} {
		e.exec(`insert into kontra.a2a_exchanges (id, at, service, method, binding, task_id, bytes_in, bytes_out, duration_ms, http_status, rpc_code, reason)
			values ($1, now() - $2::bigint * interval '1 second', 'hello', $3, 'jsonrpc', $4, 1, 2, 3, $5, $6, $7)`,
			uuid.Must(uuid.NewV7()), 10-i, x.method, x.task, x.status, x.code, x.reason)
	}
	all := items(e.json("GET", "/v1/exchanges", "", 200))
	if len(all) != 4 || all[0]["method"] != "unknown" || all[3]["method"] != "SendMessage" {
		t.Fatalf("exchanges %v", all)
	}
	if f := items(e.json("GET", "/v1/exchanges?failed=1", "", 200)); len(f) != 2 || f[0]["failed"] != true {
		t.Fatalf("failed only %v", f)
	}
	if f := items(e.json("GET", "/v1/exchanges?task="+task.String(), "", 200)); len(f) != 2 {
		t.Fatalf("by task %v", f)
	}
	if f := items(e.json("GET", "/v1/exchanges?method=GetTask", "", 200)); len(f) != 1 {
		t.Fatalf("by method %v", f)
	}
	p := e.json("GET", "/v1/exchanges?limit=3", "", 200)
	if rest := items(e.json("GET", "/v1/exchanges?limit=3&before="+p["next"].(string), "", 200)); len(rest) != 1 || rest[0]["method"] != "SendMessage" {
		t.Fatalf("second page %v", rest)
	}
	e.exec(`insert into kontra.calls (id, tenant_id, service, tool, buyer_id, principal, bytes_in, bytes_out, duration_ms, is_error)
		values ($1, $2, 'tools', 'search', $3, 'anonymous', 10, 20, 30, false), ($4, $2, 'tools', 'fetch', $3, 'anonymous', 1, 2, 3, null)`,
		uuid.Must(uuid.NewV7()), e.team, e.buyer, uuid.Must(uuid.NewV7()))
	calls := items(e.json("GET", "/v1/calls?service=tools", "", 200))
	if len(calls) != 2 || calls[0]["tool"] != "fetch" || calls[0]["outcome"] != "unknown" || calls[1]["outcome"] != "ok" {
		t.Fatalf("calls %v", calls)
	}
	if c := items(e.json("GET", "/v1/calls?service=hello", "", 200)); len(c) != 0 {
		t.Fatalf("calls filter %v", c)
	}
}

// ---------------------------------------------------------------- 4.11

// Starting a task sends A2A SendMessage to the gate's own public listener (HTTP+JSON, A2A-Version 1.0, the A2UI and
// AP2 extensions the card declares, the catalogs drawn) and records the task as the console's; a reply, a cancel and
// a continuation are refused on a task another client started.
func TestPrompting(t *testing.T) {
	e := newEnv(t, nil)
	var made uuid.UUID
	e.answer = func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		msg, _ := body["message"].(map[string]any)
		ctxID, _ := msg["contextId"].(string)
		if ctxID == "" {
			ctxID = "ctx-new"
		}
		made = e.streamed(w, body, store.StateSubmitted, ctxID)
	}
	got := e.json("POST", "/v1/tasks", `{"service":"hello","text":"hi there","a2ui_catalogs":["https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"]}`, 200)
	if got["task_id"] != made.String() || got["state"] != "submitted" || got["message_id"] == "" {
		t.Fatalf("start %v", got)
	}
	r := e.gotReq[0]
	if r.URL.Path != "/agents/hello/message:stream" || r.Header.Get("A2A-Version") != "1.0" ||
		!strings.Contains(r.Header.Get("A2A-Extensions"), ExtA2UI091) || !strings.Contains(r.Header.Get("A2A-Extensions"), ExtAP2) ||
		r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		t.Fatalf("the gate saw %s %v", r.URL.Path, r.Header)
	}
	msg := e.gotMsg[0]["message"].(map[string]any)
	if msg["role"] != "ROLE_USER" || !strings.Contains(fmt.Sprint(msg["parts"]), "hi there") ||
		!strings.Contains(fmt.Sprint(msg["metadata"]), "supportedCatalogIds") ||
		e.gotMsg[0]["configuration"] != nil {
		t.Fatalf("message %v", e.gotMsg[0])
	}
	if n := e.count(`select count(*) from kontra.console_tasks where task_id = $1`, made); n != 1 {
		t.Fatal("the started task is not the console's")
	}
	if a := e.json("GET", "/v1/tasks/"+made.String(), "", 200); fmt.Sprint(a["actions"]) != "[cancel]" || a["via"] != "console" {
		t.Fatalf("started task %v", a)
	}

	// A reply needs input-required; then it names the task.
	e.json("POST", "/v1/tasks/"+made.String()+"/messages", `{"text":"more"}`, 409)
	e.exec(`update kontra.tasks set state = 'input-required' where id = $1`, made)
	reply := made
	e.answer = func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"task":{"id":%q,"contextId":"ctx-new","status":{"state":"TASK_STATE_WORKING"}}}`, reply)
	}
	e.json("POST", "/v1/tasks/"+made.String()+"/messages", `{"a2ui":{"version":"v0.9.1","action":{"name":"book","surfaceId":"s","sourceComponentId":"b","timestamp":"t","context":{}}}}`, 200)
	last := e.gotMsg[len(e.gotMsg)-1]["message"].(map[string]any)
	if last["taskId"] != made.String() || !strings.Contains(fmt.Sprint(last["parts"]), "application/a2ui+json") {
		t.Fatalf("reply %v", last)
	}

	// Another client's task: watch-only.
	foreign := e.task(store.StateInputRequired, "hi", "ctx-foreign", false)
	before := len(e.gotReq)
	for _, p := range []string{"/messages", "/cancel", "/continue"} {
		body := `{"text":"x"}`
		if p == "/cancel" {
			body = `{}`
		}
		got := e.json("POST", "/v1/tasks/"+foreign.String()+p, body, 403)
		if got["error"].(map[string]any)["code"] != "not_console_task" {
			t.Errorf("%s: %v", p, got)
		}
	}
	if len(e.gotReq) != before {
		t.Fatal("a refused action reached the gate")
	}

	// A continuation is a new task in the ended task's context.
	e.exec(`update kontra.tasks set state = 'completed', terminal_at = now() where id = $1`, made)
	e.answer = func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		msg := body["message"].(map[string]any)
		made = e.streamed(w, body, store.StateSubmitted, msg["contextId"].(string))
	}
	cont := e.json("POST", "/v1/tasks/"+reply.String()+"/continue", `{"text":"again"}`, 200)
	if cont["task_id"] != made.String() || cont["context_id"] != "ctx-new" || e.count(`select count(*) from kontra.console_tasks where task_id = $1`, made) != 1 {
		t.Fatalf("continue %v", cont)
	}
	// The gate refuses: its error is the API's.
	e.answer = func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":{"code":400,"status":"FAILED_PRECONDITION","message":"the service requires the extension x"}}`)
	}
	if got := e.json("POST", "/v1/tasks", `{"service":"hello","text":"hi"}`, 422); !strings.Contains(fmt.Sprint(got), "requires the extension") {
		t.Fatalf("refused start %v", got)
	}
	e.json("POST", "/v1/tasks", `{"service":"hello"}`, 422)
	e.json("POST", "/v1/tasks", `{"service":"nope","text":"hi"}`, 404)
	e.json("POST", "/v1/tasks", `{"service":"hello","text":"hi","extra":1}`, 422)
}

// ---------------------------------------------------------------- 4.12

// The console never reads a push configuration (its URL or token) or a work item's payload: no file of this package
// names either table, and a task with a push configuration shows none of it on any route.
func TestNeverNamesSecretTables(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"push_configs", "work_items"} {
			if bytes.Contains(b, []byte(table)) {
				t.Errorf("%s names %s", f, table)
			}
		}
	}
}

// A task with a push configuration and a work item: no console answer holds their URL, token, credentials or payload.
func TestNeverReadsSecrets(t *testing.T) {
	e := newEnv(t, nil)
	id := e.task(store.StateWorking, "hi", "ctx-p", true)
	const url, token = "https://hooks.example/secret-path", "tok-very-secret"
	e.exec(`insert into kontra.push_configs (task_id, id, config) values ($1, 'pc', $2::jsonb)`, id,
		`{"url":"`+url+`","token":"`+token+`","authentication":{"scheme":"Bearer","credentials":"cred-secret"}}`)
	e.exec(`insert into kontra.work_items (id, task_id, kind, payload) values ($1, $2, 'execute', '{"secret":"wi-secret"}')`, uuid.Must(uuid.NewV7()), id)
	var all bytes.Buffer
	for _, p := range []string{"/v1/tasks", "/v1/tasks/" + id.String(), "/v1/conversations", "/v1/conversations/hello/" + e.buyer.String() + "/ctx-p",
		"/v1/services", "/v1/services/hello", "/v1/exchanges", "/v1/info"} {
		resp := e.do("GET", p, "")
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		all.Write(b)
	}
	ev := e.do("GET", "/v1/tasks/"+id.String()+"/events", "")
	all.Write(func() []byte { ch := readSSE(t, ev.Body, 1, 5*time.Second); return []byte(fmt.Sprint(ch)) }())
	ev.Body.Close()
	for _, s := range []string{url, token, "cred-secret", "wi-secret", "hooks.example"} {
		if bytes.Contains(all.Bytes(), []byte(s)) {
			t.Errorf("a console answer holds %q", s)
		}
	}
}
