package mcprelay

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/temporalproto"
	tclient "go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
	"kontra.sh/kontra/internal/tunnelgate"
)

// Gate-owned MCP tasks end to end at package level (spec/relay.md §11.5–§11.9; M3 WP-06): a replica's handler with
// Tasks, its Temporal workers (TaskLifecycle, the real lifecycle activities on the test's database, and this
// replica's Dispatcher on kontra-direct, kontra-tunnel-<replica> and kontra-ctl-<replica>), over a development
// server (temporaltest) and the shared Postgres (pgtest).

func TestMain(m *testing.M) {
	code := m.Run()
	temporaltest.Stop()
	os.Exit(code)
}

// BackendFor implements lifecycle.BackendResolver for prepare: a service with a direct backend is direct, else
// a tunnel service.
func (s *memServices) BackendFor(_ context.Context, _ uuid.UUID, name string) (store.Backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc := s.m[name]
	switch {
	case svc == nil:
		return "", fmt.Errorf("no service %s", name)
	case svc.Direct != nil:
		return store.BackendDirect, nil
	}
	return store.BackendTunnel, nil
}

type taskEnv struct {
	*env
	tc tclient.Client
	lc *lifecycle.Client
}

func newTaskEnv(t *testing.T) *taskEnv {
	e := newEnv(t)
	tc := temporaltest.Client(t)
	return &taskEnv{env: e, tc: tc, lc: lifecycle.NewClient(tc)}
}

// replica is a replica that serves gate-owned tasks and runs the Temporal workers.
func (e *taskEnv) replica(name string, o replicaOpts) *replica {
	t := e.t
	o.withDeps = func(r *replica, deps *Deps, l *pg.Listener, logger *slog.Logger) {
		log := store.NewLog(e.pool)
		tasks, err := NewTasks(TasksConfig{Log: log, Waker: l, Lifecycle: e.lc,
			Hooks: lifecycle.HooksSnapshot{LifecycleQueue: lifecycle.QueueLifecycle}, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		tasks.keepAlive, tasks.reread = time.Second, time.Second
		deps.Tasks = tasks
		r.dispatcher = &Dispatcher{ReplicaID: r.id, Log: log, Registry: relay.NewRegistry(), Services: e.svcs, Tunnels: r.reg,
			Waker: l, Signals: e.lc, Logger: logger, minPoll: 50 * time.Millisecond, failFor: 5 * time.Second}
		acts := &lifecycle.LifecycleActivities{Log: log, Backends: e.svcs, Sessions: tunnelgate.NewPicker(e.pool), Logger: logger}
		w, err := lifecycle.NewWorkers(e.tc, lifecycle.WorkerConfig{ReplicaID: r.id, DrainTimeout: time.Second},
			lifecycle.Activities{Lifecycle: acts, Dispatch: r.dispatcher})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(w.Stop)
	}
	return e.env.replica(name, o)
}

// seen keeps every byte a client received, to show what never reached one.
type seen struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *seen) add(b []byte) {
	s.mu.Lock()
	s.b.Write(b)
	s.b.WriteByte('\n')
	s.mu.Unlock()
}

func (s *seen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// taskClient is a stock 2025-11-25 client of gate-owned tasks.
type taskClient struct {
	*client
	url  string
	seen *seen
}

func newTaskClient(t *testing.T, key, url string) *taskClient {
	c := &taskClient{client: newClient(t, key), url: url, seen: &seen{}}
	r := c.initialize(url)
	c.seen.add(r.body)
	// The initialized notification, as a stock client sends it (server-everything registers its task tools then).
	c.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return c
}

func (c *taskClient) post(body string) reply {
	c.t.Helper()
	r := c.do(http.MethodPost, c.url, body, nil)
	c.seen.add(r.body)
	return r
}

// taskCall is a task-augmented tools/call.
func (c *taskClient) taskCall(id int, tool string, args map[string]any) reply {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args, "task": map[string]any{"ttl": 60000}}})
	return c.post(string(b))
}

func (c *taskClient) tasks(method, taskID string, id int) reply {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": map[string]any{"taskId": taskID}})
	return c.post(string(b))
}

// created reads a CreateTaskResult: the task's id.
func created(t *testing.T, r reply) string {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("task-augmented call: %d %s", r.status, r.body)
	}
	res, _ := r.msg(t)["result"].(map[string]any)
	task, _ := res["task"].(map[string]any)
	id, _ := task["taskId"].(string)
	if _, err := uuid.Parse(id); err != nil || task["status"] != "working" || task["pollInterval"] != float64(1000) {
		t.Fatalf("not a CreateTaskResult with a gate task id: %s", r.body)
	}
	if task["createdAt"] == nil || task["lastUpdatedAt"] == nil || task["ttl"].(float64) <= 0 {
		t.Fatalf("CreateTaskResult without its times: %s", r.body)
	}
	return id
}

func status(t *testing.T, r reply) string {
	t.Helper()
	res, _ := r.msg(t)["result"].(map[string]any)
	s, _ := res["status"].(string)
	return s
}

// events reads a gate-served event stream as the events arrive.
type events struct {
	t    *testing.T
	resp *http.Response
	ch   chan *event
	seen *seen
}

// stream posts body and reads its answer as an event stream.
func (c *taskClient) stream(method, body string, hdr map[string]string) *events {
	c.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, c.url, rdr)
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", c.sid)
	req.Header.Set("MCP-Protocol-Version", mcptest.Version)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	ev := &events{t: c.t, resp: resp, ch: make(chan *event, 16), seen: c.seen}
	c.t.Cleanup(func() { _ = resp.Body.Close() })
	go func() {
		defer close(ev.ch)
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			b, _ := io.ReadAll(resp.Body)
			ev.ch <- &event{data: string(b), hasData: true}
			return
		}
		r := newSSEReader(resp.Body)
		for {
			e, err := r.next()
			if err != nil {
				return
			}
			if e.hasData {
				ev.ch <- e
			}
		}
	}()
	return ev
}

// next is the stream's next event with data, failing after within; nil when the stream ended.
func (s *events) next(within time.Duration) *event {
	s.t.Helper()
	select {
	case e, ok := <-s.ch:
		if !ok {
			return nil
		}
		s.seen.add([]byte(e.data))
		return e
	case <-time.After(within):
		s.t.Fatalf("no event within %v", within)
	}
	return nil
}

func (e *event) id() string {
	for _, l := range e.lines {
		if v, ok := strings.CutPrefix(l, "id: "); ok {
			return v
		}
	}
	return ""
}

func (e *event) msg(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(e.data), &m); err != nil {
		t.Fatalf("event data is not JSON: %q", e.data)
	}
	return m
}

func (e *taskEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// historyText is the task's workflow history as JSON.
func (e *taskEnv) historyText(taskID string) string {
	e.t.Helper()
	it := e.tc.GetWorkflowHistory(context.Background(), lifecycle.WorkflowID(a2a.TaskID(taskID)), "", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var b strings.Builder
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			e.t.Fatal(err)
		}
		js, err := temporalproto.CustomJSONMarshalOptions{}.Marshal(ev)
		if err != nil {
			e.t.Fatal(err)
		}
		b.Write(js)
	}
	return b.String()
}

// elicitText and the answers are what must never cross Temporal history (spec §14: events never enter it).
const (
	elicitText = "Which python did you mean, the language or the snake?"
	answerText = "programming-interpretation"
)

// researchRun is the double's ambiguous research task: working, one elicitation, then a report naming the
// interpretation the client chose.
func researchRun(task *mcptest.Task) {
	task.Status("working", "Gathering sources...")
	time.Sleep(200 * time.Millisecond)
	ans, ok := task.Ask("elicitation/create", map[string]any{"message": elicitText,
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"interpretation": map[string]any{"type": "string"}}}},
		60*time.Second)
	if !ok {
		task.Fail(&mcptest.RPCError{Code: -32603, Message: "no clarification"})
		return
	}
	var a struct {
		Result struct {
			Content map[string]any `json:"content"`
		} `json:"result"`
	}
	_ = json.Unmarshal(ans, &a)
	task.Status("working", "Continuing...")
	time.Sleep(100 * time.Millisecond)
	res, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text",
		"text": fmt.Sprintf("report: %v", a.Result.Content["interpretation"])}}, "isError": false})
	task.Complete(res)
}

func toolWith(name, support string) json.RawMessage {
	t := map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}
	if support != "" {
		t["execution"] = map[string]any{"taskSupport": support}
	}
	b, _ := json.Marshal(t)
	return b
}

// durableFlow is the acceptance flow of a gate-owned task with one elicitation (spec §11.5–§11.8), against the
// double behind url: the CreateTaskResult with a gate UUID, tasks/get progressing, exactly one elicitation on
// tasks/result's stream with id k.<taskId>.<seq>, the client's answer reaching the upstream once (a second answer
// is 202 and dropped), the result with the related-task key rewritten, a calls row with task_id, and the upstream
// task id in no byte a client received. It returns the gate task id and the client.
func durableFlow(t *testing.T, e *taskEnv, up *mcptest.Server, key, url string) (string, *taskClient) {
	t.Helper()
	c := newTaskClient(t, key, url)
	init := c.seen.String()
	if !strings.Contains(init, `"tasks":{`) || !strings.Contains(init, `"list":{}`) || !strings.Contains(init, `"cancel":{}`) {
		t.Fatalf("a durable service advertises the upstream's tasks plus list and cancel: %s", init)
	}
	taskID := created(t, c.taskCall(7, "research", map[string]any{"topic": "python", "ambiguous": true}))
	eventually(t, 15*time.Second, "tasks/get shows input_required", func() bool {
		return status(t, c.tasks("tasks/get", taskID, 8)) == "input_required"
	})

	res := c.stream(http.MethodPost, fmt.Sprintf(`{"jsonrpc":"2.0","id":9,"method":"tasks/result","params":{"taskId":%q}}`, taskID), nil)
	ev := res.next(15 * time.Second)
	m := ev.msg(t)
	gateID, _ := m["id"].(string)
	tid, seq, ok := store.ParseGateRequestID(gateID)
	if !ok || tid.String() != taskID || m["method"] != "elicitation/create" || ev.id() != gateID {
		t.Fatalf("the first event is not the elicitation under its gate id: id %q, %s", ev.id(), ev.data)
	}
	params, _ := m["params"].(map[string]any)
	meta, _ := params["_meta"].(map[string]any)
	rel, _ := meta["io.modelcontextprotocol/related-task"].(map[string]any)
	if rel["taskId"] != taskID || params["message"] != elicitText {
		t.Fatalf("the elicitation's related-task key is not the gate task id: %s", ev.data)
	}
	if n := e.count(`select count(*) from kontra.task_events where task_id = $1 and kind = 'input_request'`, taskID); n != 1 {
		t.Fatalf("%d input_request events, want 1", n)
	}
	if n := e.count(`select count(*) from kontra.task_events where task_id = $1 and kind = 'input_request' and seq = $2`, taskID, seq); n != 1 {
		t.Fatalf("the gate id's seq %d is not the input_request event's", seq)
	}

	answer := func(interp string) reply {
		// The answer carries the related-task key, with the task id the client knows (the gate's, §11.6).
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": gateID,
			"result": map[string]any{"action": "accept", "content": map[string]any{"interpretation": interp},
				"_meta": map[string]any{"io.modelcontextprotocol/related-task": map[string]any{"taskId": taskID}}}})
		return c.post(string(b))
	}
	if r := answer(answerText); r.status != http.StatusAccepted {
		t.Fatalf("the answer: %d %s", r.status, r.body)
	}
	if r := answer("the-snake"); r.status != http.StatusAccepted {
		t.Fatalf("a second answer: %d %s, want 202", r.status, r.body)
	}
	final := res.next(20 * time.Second)
	fm := final.msg(t)
	if fm["id"] != float64(9) || final.id() == "" {
		t.Fatalf("the last event does not answer tasks/result: %s", final.data)
	}
	fr, _ := fm["result"].(map[string]any)
	fmeta, _ := fr["_meta"].(map[string]any)
	frel, _ := fmeta["io.modelcontextprotocol/related-task"].(map[string]any)
	if frel["taskId"] != taskID || !strings.Contains(final.data, "report: "+answerText) {
		t.Fatalf("tasks/result: %s", final.data)
	}
	if res.next(5*time.Second) != nil {
		t.Fatal("the stream goes on after the answer")
	}
	if n := e.count(`select count(*) from kontra.task_events where task_id = $1 and kind = 'input_response'`, taskID); n != 1 {
		t.Fatalf("%d input_response events, want 1 (the second answer is dropped)", n)
	}
	// The upstream received the client's answer once, under its own id.
	tasks := up.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("%d upstream tasks", len(tasks))
	}
	if n := up.Answers("1001"); n < 1 {
		t.Fatalf("the upstream received no answer under its id 1001")
	}
	var answers []string
	for _, r := range up.Requests() {
		if r.Method == "" && r.HTTPMethod == http.MethodPost && len(r.ID) > 0 {
			answers = append(answers, string(r.Body))
		}
	}
	if len(answers) != 1 || !strings.Contains(answers[0], answerText) || strings.Contains(answers[0], "the-snake") {
		t.Fatalf("the upstream's answers: %q", answers)
	}
	if !strings.Contains(answers[0], tasks[0].ID) || strings.Contains(answers[0], taskID) {
		t.Errorf("the answer's related-task key is not the upstream's task id: %s", answers[0])
	}
	if s := status(t, c.tasks("tasks/get", taskID, 10)); s != "completed" {
		t.Fatalf("tasks/get after the end: %s", s)
	}
	// One calls row, with task_id set (§11.5 step 6).
	var callsN int
	for _, cr := range e.calls("research") {
		if cr.task != nil && cr.task.String() == taskID {
			callsN++
			if cr.tool != "research" || cr.isError == nil || *cr.isError || cr.bytesIn == 0 || cr.bytesOut == 0 {
				t.Errorf("calls row %+v", cr)
			}
		}
	}
	if callsN != 1 {
		t.Fatalf("%d calls rows for the task, want 1", callsN)
	}
	// The upstream's task id reaches no client (§11.6).
	if strings.Contains(c.seen.String(), tasks[0].ID) || strings.Contains(c.seen.String(), "up-task-") {
		t.Fatalf("the upstream task id reached the client")
	}
	// Nothing relayed enters Temporal history (spec §14).
	h := e.historyText(taskID)
	for _, s := range []string{elicitText, answerText, "report: ", "Gathering sources"} {
		if strings.Contains(h, s) {
			t.Errorf("the workflow history holds %q", s)
		}
	}
	return taskID, c
}

// The acceptance flow against the Go double as a direct backend.
func TestDurableTaskDirect(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional")}
	up.TaskRun = researchRun
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	e.service("research", srv.URL, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	durableFlow(t, e, up, key, r.url+"/mcp/research")
}

// The same flow through a tunnel session (a FakeNode): every exchange of the dispatch carries the task, the
// creating session's hash and the creating call's id in its preamble (tunnel.md §8.1), and the upstream task id
// never appears in a client-visible byte.
func TestDurableTaskThroughTheTunnel(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "required")}
	up.TaskRun = researchRun
	e.service("research", "", nil)
	r := e.replica("a", replicaOpts{})
	n := e.node(r, up, "research")
	key, _ := e.apiKey("buyer")
	taskID, c := durableFlow(t, e, up, key, r.url+"/mcp/research")
	h := HashID(c.sid)
	session := hex.EncodeToString(h[:])
	var mine int
	for _, p := range n.fake.Preambles() {
		if p.Task != taskID {
			continue
		}
		mine++
		if p.MCPSession != session || p.MCPRequestID != "7" || p.Context == "" {
			t.Errorf("an exchange of the task's dispatch has the preamble %+v", p)
		}
	}
	// initialize, notifications/initialized, tools/call, tasks/get…, tasks/result, the answer, the DELETE.
	if mine < 6 {
		t.Fatalf("%d exchanges named the task", mine)
	}
	var a struct{ upstream string }
	if err := e.pool.QueryRow(context.Background(), `select coalesce(upstream_session, '') from kontra.attempts where task_id = $1 and n = 1`, taskID).Scan(&a.upstream); err != nil || a.upstream == "" {
		t.Fatalf("attempts.upstream_session: %q %v", a.upstream, err)
	}
	if a.upstream == e.row(c.sid).upstreamSID {
		t.Fatal("the dispatch rode the client's upstream session (D22)")
	}
}

// Refusals before any task (§11.5 step 0): a tool whose taskSupport is forbidden or absent is -32601, a denied or
// unknown tool -32602; neither consults Admission nor creates a task.
func TestDurableTaskRefusals(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional"), toolWith("never", "forbidden"), toolWith("plain", ""),
		toolWith("secret", "required")}
	up.TaskRun = researchRun
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	e.service("research", srv.URL, func(s *Service) { s.Tools = Policy{Deny: []string{"secret"}} })
	adm := &fakeAdmission{}
	r := e.replica("a", replicaOpts{admission: adm, ops: []hooks.Operation{hooks.OpTask}})
	key, _ := e.apiKey("buyer")
	c := newTaskClient(t, key, r.url+"/mcp/research")
	for tool, code := range map[string]int{"never": -32601, "plain": -32601, "secret": -32602, "nosuch": -32602} {
		if got := c.taskCall(3, tool, map[string]any{}).errCode(t); got != code {
			t.Errorf("%s: %d, want %d", tool, got, code)
		}
	}
	if n := e.count(`select count(*) from kontra.tasks`); n != 0 || adm.n() != 0 {
		t.Fatalf("%d tasks, Admission asked %d times", n, adm.n())
	}
	if up.Count("tools/call") != 0 {
		t.Fatal("a refused call reached the upstream")
	}
	// Admission denies: no task either.
	adm.set(hooks.Decision{Outcome: hooks.Deny, Refusal: &hooks.Refusal{Code: -32003, Message: "no credit"}}, nil)
	if got := c.taskCall(4, "research", map[string]any{}).errCode(t); got != -32003 {
		t.Errorf("a denied task: %d", got)
	}
	// Admission quotes: the task is created quoted, working to the client with the approve URL (D21).
	adm.set(hooks.Decision{Outcome: hooks.Quote, MCPMeta: map[string]any{"sh.kontra/price": "1 USD"},
		Quote: &hooks.QuoteTerms{ApproveURL: "https://pay.example/t/1", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}, nil)
	rq := c.taskCall(5, "research", map[string]any{})
	taskID := created(t, rq)
	if !strings.Contains(string(rq.body), `"statusMessage":"https://pay.example/t/1"`) || !strings.Contains(string(rq.body), `"sh.kontra/approve-url":"https://pay.example/t/1"`) ||
		!strings.Contains(string(rq.body), `"sh.kontra/price":"1 USD"`) {
		t.Fatalf("a quoted task: %s", rq.body)
	}
	var st string
	_ = e.pool.QueryRow(context.Background(), `select state from kontra.tasks where id = $1`, taskID).Scan(&st)
	if st != "quoted" || up.Count("tools/call") != 0 {
		t.Fatalf("a quoted task is %s and dispatched %d calls", st, up.Count("tools/call"))
	}
	g := c.tasks("tasks/get", taskID, 6)
	if status(t, g) != "working" || !strings.Contains(string(g.body), `"sh.kontra/approve-url"`) {
		t.Fatalf("tasks/get of a quoted task: %s", g.body)
	}
}

// tasks/cancel on a working task returns status cancelled within 10 s even when the upstream ignores the cancel
// (D57), and a later upstream completion is not logged (§5.1's terminal rule).
func TestDurableTaskCancelIgnoredUpstream(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional")}
	up.IgnoreCancel = true
	finish := make(chan struct{})
	up.TaskRun = func(task *mcptest.Task) {
		task.Status("working", "a long job")
		<-finish
		res, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": "done late"}}, "isError": false})
		task.Complete(res)
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	})
	e.service("research", srv.URL, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newTaskClient(t, key, r.url+"/mcp/research")
	taskID := created(t, c.taskCall(7, "research", map[string]any{}))
	eventually(t, 15*time.Second, "the dispatch has the upstream task", func() bool {
		return e.count(`select count(*) from kontra.attempts where task_id = $1 and upstream_task_id is not null`, taskID) == 1 &&
			status(t, c.tasks("tasks/get", taskID, 8)) == "working"
	})
	began := time.Now()
	cr := c.tasks("tasks/cancel", taskID, 9)
	took := time.Since(began)
	if status(t, cr) != "cancelled" || took > 10*time.Second {
		t.Fatalf("tasks/cancel answered %s after %v: %s", status(t, cr), took, cr.body)
	}
	if up.Count("tasks/cancel") != 1 {
		t.Errorf("the upstream received %d tasks/cancel (it declared tasks.cancel)", up.Count("tasks/cancel"))
	}
	close(finish)
	time.Sleep(2 * time.Second)
	if n := e.count(`select count(*) from kontra.task_events where task_id = $1 and kind = 'result'`, taskID); n != 0 {
		t.Fatalf("a completion after the cancel was logged")
	}
	if s := status(t, c.tasks("tasks/get", taskID, 10)); s != "cancelled" {
		t.Fatalf("tasks/get: %s", s)
	}
	res := c.tasks("tasks/result", taskID, 11)
	m := res.msg(t)
	er, _ := m["error"].(map[string]any)
	data, _ := er["data"].(map[string]any)
	if er["code"] != float64(-32603) || er["message"] != "task cancelled" || data["sh.kontra/canceled"] != "client" {
		t.Fatalf("tasks/result of a cancelled task: %s", res.body)
	}
	if got := c.tasks("tasks/cancel", taskID, 12).errCode(t); got != -32602 {
		t.Fatalf("a second cancel: %d, want -32602", got)
	}
	var calls []callRow
	for _, cr := range e.calls("research") {
		if cr.task != nil && cr.task.String() == taskID {
			calls = append(calls, cr)
		}
	}
	if len(calls) != 1 || calls[0].isError == nil || !*calls[0].isError {
		t.Fatalf("calls rows of a cancelled task: %+v (one, is_error true, §11.8)", calls)
	}
}

// Another buyer's tasks/get, tasks/result and tasks/cancel are -32602, as for an unknown task; tasks/list lists the
// buyer's own only, newest first; an anonymous caller's tasks/list is -32601.
func TestDurableTaskScope(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional")}
	up.TaskRun = func(task *mcptest.Task) {
		res, _ := json.Marshal(map[string]any{"content": []any{}, "isError": false})
		task.Complete(res)
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	allow, err := auth.ParseAllowList([]string{"org:team", "anonymous"})
	if err != nil {
		t.Fatal(err)
	}
	e.service("research", srv.URL, func(s *Service) { s.Allow = allow })
	r := e.replica("a", replicaOpts{})
	keyA, _ := e.apiKey("a")
	keyB, _ := e.apiKey("b")
	a := newTaskClient(t, keyA, r.url+"/mcp/research")
	b := newTaskClient(t, keyB, r.url+"/mcp/research")
	t1 := created(t, a.taskCall(1, "research", map[string]any{}))
	t2 := created(t, a.taskCall(2, "research", map[string]any{}))
	tb := created(t, b.taskCall(3, "research", map[string]any{}))
	for _, m := range []string{"tasks/get", "tasks/result", "tasks/cancel"} {
		if got := b.tasks(m, t1, 4).errCode(t); got != -32602 {
			t.Errorf("another buyer's %s: %d", m, got)
		}
		if got := a.tasks(m, "not-a-uuid", 4).errCode(t); got != -32602 {
			t.Errorf("%s of a malformed id: %d", m, got)
		}
	}
	l := a.post(`{"jsonrpc":"2.0","id":5,"method":"tasks/list","params":{}}`)
	var list struct {
		Result struct {
			Tasks []struct {
				TaskID string `json:"taskId"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(l.body, &list); err != nil || len(list.Result.Tasks) != 2 || list.Result.Tasks[0].TaskID != t2 || list.Result.Tasks[1].TaskID != t1 {
		t.Fatalf("tasks/list: %s", l.body)
	}
	if strings.Contains(string(l.body), tb) {
		t.Fatal("tasks/list shows another buyer's task")
	}
	anon := newTaskClient(t, "", r.url+"/mcp/research")
	if got := anon.post(`{"jsonrpc":"2.0","id":6,"method":"tasks/list","params":{}}`).errCode(t); got != -32601 {
		t.Fatalf("anonymous tasks/list: %d", got)
	}
}

// A GET whose Last-Event-ID is k.<taskId>.<n> resumes on the other replica for the same principal (served from
// task_events, never forwarded), and is 404 for another principal (§11.7).
func TestDurableTaskResumeOnTheOtherReplica(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional")}
	up.TaskRun = researchRun
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	e.service("research", srv.URL, nil)
	ra := e.replica("a", replicaOpts{})
	rb := e.replica("b", replicaOpts{})
	keyA, _ := e.apiKey("a")
	keyB, _ := e.apiKey("b")
	a := newTaskClient(t, keyA, ra.url+"/mcp/research")
	taskID := created(t, a.taskCall(7, "research", map[string]any{"topic": "python"}))
	eventually(t, 15*time.Second, "the input request is logged", func() bool {
		return e.count(`select count(*) from kontra.task_events where task_id = $1 and kind = 'input_request'`, taskID) == 1
	})
	var seq int64
	_ = e.pool.QueryRow(context.Background(), `select seq from kontra.task_events where task_id = $1 and kind = 'input_request'`, taskID).Scan(&seq)
	// The same client, through replica b.
	onB := &taskClient{client: a.client, url: rb.url + "/mcp/research", seen: a.seen}
	ev := onB.stream(http.MethodGet, "", map[string]string{"Last-Event-ID": store.GateRequestID(uuid.MustParse(taskID), seq-1)})
	if ev.resp.StatusCode != http.StatusOK {
		t.Fatalf("resume: %d", ev.resp.StatusCode)
	}
	got := ev.next(10 * time.Second)
	if got.id() != store.GateRequestID(uuid.MustParse(taskID), seq) || got.msg(t)["method"] != "elicitation/create" {
		t.Fatalf("the resumed stream: %q %s", got.id(), got.data)
	}
	for _, q := range up.Requests() {
		if strings.HasPrefix(q.Header.Get("Last-Event-ID"), "k.") {
			t.Error("a gate-served resume reached the upstream")
		}
	}
	// Another principal, its own session on b.
	b := newTaskClient(t, keyB, rb.url+"/mcp/research")
	if r := b.do(http.MethodGet, b.url, "", map[string]string{"Last-Event-ID": store.GateRequestID(uuid.MustParse(taskID), 1)}); r.status != http.StatusNotFound {
		t.Fatalf("another principal's resume: %d", r.status)
	}
	if r := b.do(http.MethodGet, b.url, "", map[string]string{"Last-Event-ID": "k.not-a-task.1"}); r.status != http.StatusNotFound {
		t.Fatalf("a malformed gate event id: %d", r.status)
	}
}

// Sealed tasks (B3, B4; corrections WP-06): on a sealed service the gate never alters an envelope: the input_request
// event and the bytes the client receives on tasks/result carry the upstream's envelope byte for byte; the answer
// the double receives carries the client's envelope byte for byte under the double's original id. An upstream
// tasks/result with {"isError": false, "ISERROR": true} fails the task upstream_error (D172).
func TestDurableTaskSealedEnvelopes(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional"), toolWith("smuggle", "optional")}
	const upEnvelope = `{"v":1,"envelope":"QUJDRA=="}`
	const clientEnvelope = `{"v":1,"envelope":"WFlaVw=="}`
	up.TaskRun = func(task *mcptest.Task) {
		if strings.Contains(string(task.Call.Params), "smuggle") {
			task.Complete(json.RawMessage(`{"content":[],"isError":false,"ISERROR":true}`))
			return
		}
		msg := []byte(`{"jsonrpc":"2.0","id":4242,"method":"elicitation/create","params":{"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"` +
			task.ID + `"},"sh.kontra/sealed":` + upEnvelope + `}}}`)
		ans, ok := task.AskRaw("4242", msg, 60*time.Second)
		if !ok {
			task.Fail(&mcptest.RPCError{Code: -32603, Message: "no answer"})
			return
		}
		if !bytes.Contains(ans, []byte(`"sh.kontra/sealed":`+clientEnvelope)) || !bytes.Contains(ans, []byte(`"id":4242`)) {
			task.Fail(&mcptest.RPCError{Code: -32603, Message: "the answer's envelope was altered: " + string(ans)})
			return
		}
		task.Complete(json.RawMessage(`{"content":[{"type":"resource","resource":{"uri":"kontra:sealed","blob":"QUJD"}}],"isError":false}`))
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	e.service("research", srv.URL, func(s *Service) { s.Sealed = true })
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newTaskClient(t, key, r.url+"/mcp/research")
	taskID := created(t, c.taskCall(7, "research", map[string]any{}))
	res := c.stream(http.MethodPost, fmt.Sprintf(`{"jsonrpc":"2.0","id":9,"method":"tasks/result","params":{"taskId":%q}}`, taskID), nil)
	ev := res.next(15 * time.Second)
	if !strings.Contains(ev.data, `"sh.kontra/sealed":`+upEnvelope) {
		t.Fatalf("the client's elicitation does not carry the upstream's envelope byte for byte: %s", ev.data)
	}
	var payload string
	if err := e.pool.QueryRow(context.Background(), `select payload::text from kontra.task_events where task_id = $1 and kind = 'input_request'`, taskID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"sh.kontra/sealed":`+upEnvelope) || !strings.Contains(payload, `"upstream":{"id":4242}`) {
		t.Fatalf("the input_request event: %s", payload)
	}
	gateID, _ := ev.msg(t)["id"].(string)
	if r := c.post(`{"jsonrpc":"2.0","id":"` + gateID + `","result":{"_meta":{"sh.kontra/sealed":` + clientEnvelope + `}}}`); r.status != http.StatusAccepted {
		t.Fatalf("the sealed answer: %d", r.status)
	}
	final := res.next(20 * time.Second)
	if !strings.Contains(final.data, `"kontra:sealed"`) || strings.Contains(final.data, `"error"`) {
		t.Fatalf("the sealed task's result: %s", final.data)
	}

	smug := created(t, c.taskCall(8, "smuggle", map[string]any{}))
	eventually(t, 15*time.Second, "the smuggled isError fails the task", func() bool { return status(t, c.tasks("tasks/get", smug, 10)) == "failed" })
	g := c.tasks("tasks/get", smug, 11)
	if !strings.Contains(string(g.body), `"sh.kontra/failure":"upstream_error"`) {
		t.Fatalf("tasks/get: %s", g.body)
	}
	rr := c.tasks("tasks/result", smug, 12)
	if rr.errCode(t) != -32603 || strings.Contains(string(rr.body), "ISERROR") {
		t.Fatalf("tasks/result of the smuggled result: %s", rr.body)
	}
}

// A dispatch's write after the task ended is refused, and a dispatch whose fence a newer one took stops: the
// store's rules the dispatch relies on (D77, §5.1).
func TestDispatchFenceAndTerminalRule(t *testing.T) {
	e := newTaskEnv(t)
	log := store.NewLog(e.pool)
	ctx := context.Background()
	e.service("research", "http://127.0.0.1:1", nil)
	id := uuid.Must(uuid.NewV7())
	svc, err := log.LoadService(ctx, e.team, "research")
	if err != nil {
		t.Fatal(err)
	}
	nt, err := store.NewMCPTask(id, svc, store.Owner{BuyerID: uuid.New(), Principal: "key:x", Via: store.ViaAPIKey}, store.StateSubmitted, nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.CreateTask(ctx, nil, nt); err != nil {
		t.Fatal(err)
	}
	if err := log.BeginAttempt(ctx, store.Attempt{TaskID: id, N: 1, Backend: store.BackendDirect}); err != nil {
		t.Fatal(err)
	}
	f1, err := log.BeginDispatch(ctx, id, 1, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendFenced(ctx, log, f1, func(row *store.TaskRow) (store.Write, error) {
		return store.MCPStatusWrite(row, store.StateWorking, "w", nil)
	}); err != nil {
		t.Fatal(err)
	}
	f2, err := log.BeginDispatch(ctx, id, 1, "r1")
	if err != nil {
		t.Fatal(err)
	}
	err = appendFenced(ctx, log, f1, func(row *store.TaskRow) (store.Write, error) {
		return store.MCPStatusWrite(row, store.StateInputRequired, "", nil)
	})
	if !errors.Is(err, store.ErrFenced) || !f1.Fenced() {
		t.Fatalf("a superseded dispatch's write: %v", err)
	}
	if _, err := log.WriteTerminal(ctx, store.TerminalWrite{TaskID: id, State: store.StateCanceled, Canceled: store.CanceledClient}); err != nil {
		t.Fatal(err)
	}
	err = appendFenced(ctx, log, f2, func(row *store.TaskRow) (store.Write, error) {
		return store.MCPResultWrite(row, store.StateCompleted, "result", json.RawMessage(`{"content":[]}`), nil)
	})
	if !errors.Is(err, errTaskEnded) {
		t.Fatalf("a result after canceled: %v", err)
	}
	if n := e.count(`select count(*) from kontra.calls where task_id = $1`, id); n != 1 {
		t.Fatalf("%d calls rows, want the cancel's one", n)
	}
}

// tasks/cancel is answered cancelled within 10 s when the upstream never answers its tasks/cancel (D57: the gate
// waits at most 10 s, then writes canceled whatever the upstream does), and when the cancel comes before the first
// upstream task id while the upstream hangs on the call (abort: the dispatch is stopped in process, spec §8).
func TestDurableTaskCancelHangingUpstream(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional"), toolWith("stuck", "optional")}
	release := make(chan struct{})
	up.Hold = func(r mcptest.Request) {
		if r.Method == "tasks/cancel" || (r.Method == "tools/call" && strings.Contains(string(r.Params), `"stuck"`)) {
			select {
			case <-release:
			case <-time.After(40 * time.Second):
			}
		}
	}
	up.TaskRun = func(task *mcptest.Task) { task.Status("working", "a long job") }
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // before the server's Close: a held request ends first
	e.service("research", srv.URL, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newTaskClient(t, key, r.url+"/mcp/research")

	t.Run("after the first upstream id", func(t *testing.T) {
		taskID := created(t, c.taskCall(7, "research", map[string]any{}))
		eventually(t, 15*time.Second, "the dispatch has the upstream task", func() bool {
			return e.count(`select count(*) from kontra.attempts where task_id = $1 and upstream_task_id is not null`, taskID) == 1
		})
		began := time.Now()
		cr := c.tasks("tasks/cancel", taskID, 9)
		if took := time.Since(began); status(t, cr) != "cancelled" || took >= 10*time.Second {
			t.Fatalf("tasks/cancel answered %s after %v: %s", status(t, cr), took, cr.body)
		}
		eventually(t, 5*time.Second, "the upstream received the cancel", func() bool { return up.Count("tasks/cancel") == 1 })
		var outcome string
		eventually(t, 20*time.Second, "the attempt ended", func() bool {
			_ = e.pool.QueryRow(context.Background(), `select coalesce(outcome, '') from kontra.attempts where task_id = $1 and n = 1`, taskID).Scan(&outcome)
			return outcome != ""
		})
		if outcome != "canceled" {
			t.Fatalf("attempt outcome %q", outcome)
		}
	})
	t.Run("before the first upstream id", func(t *testing.T) {
		taskID := created(t, c.taskCall(8, "stuck", map[string]any{}))
		eventually(t, 15*time.Second, "the dispatch sent the call", func() bool { return up.Calls("stuck") == 1 })
		began := time.Now()
		cr := c.tasks("tasks/cancel", taskID, 10)
		if took := time.Since(began); status(t, cr) != "cancelled" || took >= 10*time.Second {
			t.Fatalf("tasks/cancel answered %s after %v: %s", status(t, cr), took, cr.body)
		}
		var outcome string
		eventually(t, 20*time.Second, "the attempt ended", func() bool {
			_ = e.pool.QueryRow(context.Background(), `select coalesce(outcome, '') from kontra.attempts where task_id = $1 and n = 1`, taskID).Scan(&outcome)
			return outcome != ""
		})
		if outcome != "aborted" {
			t.Fatalf("attempt outcome %q, want aborted", outcome)
		}
	})
}

// startEverything installs @modelcontextprotocol/server-everything@1.0.0 from examples/mcp/everything's
// package-lock and runs it (streamableHttp) on a free loopback port, as that example's run.sh does (D185: bound to
// 127.0.0.1 by loopback.cjs, a minimal environment, GZIP_ALLOWED_DOMAINS set); its process group is killed when the
// test ends. The test is skipped, saying why, when node or npm is missing or npm cannot install the pin.
func startEverything(t *testing.T) string {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("server-everything@1.0.0: node is not on PATH")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("server-everything@1.0.0: npm is not on PATH")
	}
	src := filepath.Join("..", "..", "examples", "mcp", "everything")
	dir := t.TempDir()
	for _, f := range []string{"package.json", "package-lock.json", "loopback.cjs"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	install := exec.CommandContext(ctx, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund")
	install.Dir = dir
	install.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "npm_config_cache=" + filepath.Join(dir, ".npm"), "npm_config_update_notifier=false"}
	if out, err := install.CombinedOutput(); err != nil {
		t.Skipf("server-everything@1.0.0: npm cannot install the pinned package here: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cmd := exec.Command(nodeBin, filepath.Join(dir, "node_modules", "@modelcontextprotocol", "server-everything", "dist", "index.js"), "streamableHttp")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LANG=C.UTF-8", "PORT=" + strconv.Itoa(port),
		"GZIP_ALLOWED_DOMAINS=example.invalid", "NODE_OPTIONS=--require=" + filepath.Join(dir, "loopback.cjs")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("server-everything:\n%s", logs.String())
		}
	})
	eventually(t, 30*time.Second, "server-everything listens", func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
}

// The acceptance flow against server-everything@1.0.0 as a direct backend (spec §11.5; D22): simulate-research-query
// with params.task returns a CreateTaskResult with a gate UUID taskId; tasks/get progresses; the ambiguous topic
// produces exactly one elicitation on tasks/result's stream, with an id k.<taskId>.<seq>; the client's answer reaches
// the upstream once (a second answer is 202 and dropped); tasks/result returns the upstream's result with the
// related-task key rewritten to the gate id; a calls row has task_id set.
func TestDurableTaskServerEverything(t *testing.T) {
	url := startEverything(t)
	e := newTaskEnv(t)
	e.service("everything", url, func(s *Service) { s.Tools = Policy{Deny: []string{"get-env", "gzip-file-as-resource"}} })
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newTaskClient(t, key, r.url+"/mcp/everything")
	if !strings.Contains(c.seen.String(), `"cancel":{}`) {
		t.Fatalf("initialize: %s", c.seen.String())
	}
	if l := c.post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`); !strings.Contains(string(l.body), "simulate-research-query") {
		t.Fatalf("tools/list: %s", l.body)
	}
	taskID := created(t, c.taskCall(7, "simulate-research-query", map[string]any{"topic": "python", "ambiguous": true}))
	states := map[string]bool{}
	eventually(t, 30*time.Second, "tasks/get shows input_required", func() bool {
		s := status(t, c.tasks("tasks/get", taskID, 8))
		states[s] = true
		return s == "input_required"
	})
	if !states["working"] {
		t.Errorf("tasks/get never showed working: %v", states)
	}
	res := c.stream(http.MethodPost, fmt.Sprintf(`{"jsonrpc":"2.0","id":9,"method":"tasks/result","params":{"taskId":%q}}`, taskID), nil)
	ev := res.next(30 * time.Second)
	m := ev.msg(t)
	gateID, _ := m["id"].(string)
	if tid, _, ok := store.ParseGateRequestID(gateID); !ok || tid.String() != taskID || m["method"] != "elicitation/create" || ev.id() != gateID {
		t.Fatalf("the first event is not the elicitation under its gate id: %q %s", ev.id(), ev.data)
	}
	if !strings.Contains(ev.data, `"io.modelcontextprotocol/related-task":{"taskId":"`+taskID+`"}`) {
		t.Fatalf("the elicitation's related-task key is not the gate's: %s", ev.data)
	}
	answer := func(interp string) int {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": gateID,
			"result": map[string]any{"action": "accept", "content": map[string]any{"interpretation": interp}}})
		return c.post(string(b)).status
	}
	if s := answer("programming"); s != http.StatusAccepted {
		t.Fatalf("the answer: %d", s)
	}
	if s := answer("snake"); s != http.StatusAccepted {
		t.Fatalf("a second answer: %d", s)
	}
	final := res.next(60 * time.Second)
	fm := final.msg(t)
	if fm["id"] != float64(9) || !strings.Contains(final.data, "python (programming)") ||
		!strings.Contains(final.data, `"io.modelcontextprotocol/related-task":{"taskId":"`+taskID+`"}`) {
		t.Fatalf("tasks/result: %s", final.data)
	}
	if n := e.count(`select count(*) from kontra.task_events where task_id = $1 and kind = 'input_request'`, taskID); n != 1 {
		t.Fatalf("%d elicitations logged, want exactly 1", n)
	}
	if n := e.count(`select count(*) from kontra.task_events where task_id = $1 and kind = 'input_response'`, taskID); n != 1 {
		t.Fatalf("%d answers logged, want 1 (the second is dropped)", n)
	}
	if n := strings.Count(r.logs.String(), "an answer to an input request was written upstream"); n != 1 {
		t.Fatalf("the answer was written upstream %d times", n)
	}
	if n := e.count(`select count(*) from kontra.calls where task_id = $1 and is_error = false`, taskID); n != 1 {
		t.Fatalf("%d calls rows with task_id", n)
	}
	var up string
	if err := e.pool.QueryRow(context.Background(), `select upstream_task_id from kontra.attempts where task_id = $1 and n = 1`, taskID).Scan(&up); err != nil || up == "" {
		t.Fatalf("the upstream task id: %v", err)
	}
	if strings.Contains(c.seen.String(), up) {
		t.Fatal("the upstream task id reached the client")
	}
}

// After the first upstream task id a lost tunnel fails the task tunnel_lost (D32, §11.9): the call is never sent
// again, and tasks/result returns that failure (§11.8 case 2).
func TestDurableTaskTunnelLostAfterFirstID(t *testing.T) {
	e := newTaskEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional")}
	up.TaskRun = func(task *mcptest.Task) { task.Status("working", "a long job") }
	e.service("research", "", nil)
	r := e.replica("a", replicaOpts{})
	n := e.node(r, up, "research")
	key, _ := e.apiKey("buyer")
	c := newTaskClient(t, key, r.url+"/mcp/research")
	taskID := created(t, c.taskCall(7, "research", map[string]any{}))
	eventually(t, 15*time.Second, "the dispatch has the upstream task", func() bool {
		return e.count(`select count(*) from kontra.attempts where task_id = $1 and upstream_task_id is not null`, taskID) == 1
	})
	// A second node of the service, for the client's new session once its own closes with the first node's tunnel
	// (§11.2: the client re-initializes and lands on any connected session).
	e.node(r, up, "research")
	_ = n.session.Close("test: the node went away")
	if rep := c.tasks("tasks/get", taskID, 8); rep.status != http.StatusNotFound {
		eventually(t, 5*time.Second, "the client's session closed with its tunnel", func() bool { return e.row(c.sid).closed })
	}
	c.initialize(c.url)
	eventually(t, 20*time.Second, "the task failed", func() bool { return status(t, c.tasks("tasks/get", taskID, 8)) == "failed" })
	g := c.tasks("tasks/get", taskID, 9)
	if !strings.Contains(string(g.body), `"sh.kontra/failure":"tunnel_lost"`) {
		t.Fatalf("tasks/get: %s", g.body)
	}
	res := c.tasks("tasks/result", taskID, 10).msg(t)
	er, _ := res["error"].(map[string]any)
	data, _ := er["data"].(map[string]any)
	if er["code"] != float64(-32603) || data["sh.kontra/failure"] != "tunnel_lost" {
		t.Fatalf("tasks/result: %v", res)
	}
	if up.Calls("research") != 1 {
		t.Fatalf("the upstream received %d calls: a task is never sent again after its first upstream id", up.Calls("research"))
	}
	var outcome string
	eventually(t, 20*time.Second, "the attempt ended", func() bool {
		_ = e.pool.QueryRow(context.Background(), `select coalesce(outcome, '') from kontra.attempts where task_id = $1 and n = 1`, taskID).Scan(&outcome)
		return outcome != ""
	})
	if outcome != "tunnel_lost" {
		t.Fatalf("attempt outcome %q", outcome)
	}
}

// A resumed dispatch of a direct attempt (D42, spec §6.4's re-attach) follows the recorded upstream task on the
// recorded upstream session: it never sends the call again, logs the outcome, writes the calls row, and closes the
// session.
func TestDispatchReattachesDirect(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	up := mcptest.New()
	up.Tools = []json.RawMessage{toolWith("research", "optional")}
	up.TaskRun = func(task *mcptest.Task) {
		time.Sleep(300 * time.Millisecond)
		task.Complete(json.RawMessage(`{"content":[{"type":"text","text":"finished"}],"isError":false}`))
	}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	e.service("research", srv.URL, nil)
	log := store.NewLog(e.pool)
	svc, err := log.LoadService(ctx, e.team, "research")
	if err != nil {
		t.Fatal(err)
	}
	// The upstream session and task an earlier dispatch opened.
	post := func(sid, body string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, _ := post("", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	_, body := post(sid, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"research","task":{"ttl":60000}}}`)
	var ct struct {
		Result struct {
			Task struct {
				TaskID string `json:"taskId"`
			} `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &ct); err != nil || ct.Result.Task.TaskID == "" {
		t.Fatalf("the upstream task: %s", body)
	}

	id := uuid.Must(uuid.NewV7())
	owner := store.Owner{BuyerID: uuid.New(), Principal: "key:x", Via: store.ViaAPIKey}
	nt, err := store.NewMCPTask(id, svc, owner, store.StateSubmitted, nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.CreateTask(ctx, nil, nt); err != nil {
		t.Fatal(err)
	}
	wid := uuid.Must(uuid.NewV7())
	work := &store.MCPWork{Request: []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"research"}}`), Tool: "research",
		RequestID: json.RawMessage(`7`), Version: "2025-11-25", Session: strings.Repeat("1f", 32)}
	if err := log.InsertWorkItem(ctx, nil, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute, Payload: store.WorkPayload{Owner: owner, MCP: work}}); err != nil {
		t.Fatal(err)
	}
	if err := log.ConsumeWorkItem(ctx, wid, id); err != nil {
		t.Fatal(err)
	}
	if err := log.BeginAttempt(ctx, store.Attempt{TaskID: id, N: 1, Backend: store.BackendDirect}); err != nil {
		t.Fatal(err)
	}
	f, err := log.BeginDispatch(ctx, id, 1, "gone-replica")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.SetUpstreamSession(ctx, f, sid); err != nil {
		t.Fatal(err)
	}
	if err := log.SetFirstUpstreamID(ctx, f, ct.Result.Task.TaskID, ""); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{ReplicaID: "r-resume", Log: log, Registry: relay.NewRegistry(), Services: e.svcs, minPoll: 50 * time.Millisecond}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := d.Dispatch(dctx, lifecycle.DispatchInput{TaskID: id.String(), N: 1, WorkItemID: wid.String(), Resume: true})
	if err != nil || res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("the resume: %+v %v", res, err)
	}
	row, err := log.Load(ctx, id)
	if err != nil || row.State != store.StateCompleted {
		t.Fatalf("the task: %v %v", row.State, err)
	}
	if up.Calls("research") != 1 {
		t.Fatalf("the upstream received %d calls: a re-attach never sends the call again", up.Calls("research"))
	}
	if n := e.calls("research"); len(n) != 1 || n[0].task == nil || *n[0].task != id {
		t.Fatalf("calls rows: %+v", n)
	}
	if len(up.Sessions()) != 0 {
		t.Fatalf("the dispatch's upstream session was not closed: %v", up.Sessions())
	}
	a, err := log.GetAttempt(ctx, id, 1)
	if err != nil || a.Outcome != store.OutcomeCompleted || a.ReplicaID != "r-resume" {
		t.Fatalf("the attempt: %+v %v", a, err)
	}
}
