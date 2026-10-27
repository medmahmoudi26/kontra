package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
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

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/store"
)

// Regression tests for the review of M3 WP-07 (the 2026-07-28 surface).

// The era is dropped when a node of the service connects or disconnects (relay §11.12, D177), on every replica:
// through the peer hop each replica uses the nodes another one holds. Replica A learns legacy-only through the hop
// to a node on replica B; the node leaves B and a dual-era node connects to B; A drops the era both times (NOTIFY
// kontra_tunnel 'mcp:…'), and its next modern request probes the new node and is forwarded. B applies each change
// once (its own NOTIFY is not applied twice).
func TestModernEraDroppedOnEveryReplica(t *testing.T) {
	e := newEnv(t)
	legacy := mcptest.New()
	legacy.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	e.service("tun", "", nil)
	ra := e.replica("a", replicaOpts{peer: true})
	rb := e.replica("b", replicaOpts{peer: true})
	n1 := e.node(rb, legacy, "tun")
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, ra.url+"/mcp/tun", false)
	if rep := m.req(1, "tools/list", nil); rep.status != http.StatusBadRequest || !strings.Contains(string(rep.body), "No valid session ID") {
		t.Fatalf("legacy-only through the hop: %d %s", rep.status, rep.body)
	}
	k := ToolsKey{TenantID: e.team, Service: "tun"}
	if era, known, _ := ra.tools.Era(context.Background(), k, nil); !known || era != mcpwire.EraLegacy {
		t.Fatalf("A did not learn legacy-only through the hop: era %v known %v", era, known)
	}
	dropsA, dropsB := ra.eraDrops.Load(), rb.eraDrops.Load()
	_ = n1.gateMux.Close()
	eventually(t, 10*time.Second, "A drops the era when the node leaves B", func() bool { return ra.eraDrops.Load() > dropsA })
	if _, known, _ := ra.tools.Era(context.Background(), k, nil); known {
		t.Fatal("A still knows the era after the node left B")
	}
	modern := mcptest.New()
	modern.Modern = true
	modern.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	dropsA = ra.eraDrops.Load()
	e.node(rb, modern, "tun")
	eventually(t, 10*time.Second, "A drops the era when a node connects to B", func() bool { return ra.eraDrops.Load() > dropsA })
	rep := m.req(2, "tools/list", nil)
	res := result(t, rep)
	if tools, _ := res["tools"].([]any); len(tools) != 1 || modern.Count("server/discover") != 1 {
		t.Fatalf("A after the dual-era node connected to B: %s; probes at the new node: %d", rep.body, modern.Count("server/discover"))
	}
	time.Sleep(500 * time.Millisecond) // B's own NOTIFYs have come back by now
	if got := rb.eraDrops.Load() - dropsB; got != 2 {
		t.Errorf("B applied %d era drops for one disconnect and one connect", got)
	}
}

// A modern client polling its gate-owned task is sending requests: tasks/get touches its principal's virtual
// session, which the idle sweep then keeps (relay §11.12 "per_session": closed idle after session_idle_s without a
// request; last_seen_at written at most once a minute).
func TestModernTaskPollingKeepsVirtualSession(t *testing.T) {
	e := newTaskEnv(t)
	_, url := modernTaskDouble(t)
	e.service("research", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/research", true)
	taskID := createdModern(t, m.req(1, "tools/call", map[string]any{"name": "long", "arguments": map[string]any{}}))
	rows := e.virtualRows("research")
	if len(rows) != 1 {
		t.Fatalf("virtual rows %+v", rows)
	}
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `update kontra.mcp_sessions set last_seen_at = now() - interval '2 minutes' where service = 'research'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `update kontra.services set session_idle_s = 60 where name = 'research'`); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		result(t, m.req(10+i, "tasks/get", map[string]any{"taskId": taskID}))
	}
	var age float64
	if err := e.pool.QueryRow(ctx, `select extract(epoch from now() - last_seen_at) from kontra.mcp_sessions where client_sid_hash = $1`,
		rows[0].hash).Scan(&age); err != nil {
		t.Fatal(err)
	}
	if n, err := r.h.SweepIdle(ctx); err != nil || n != 0 || age > 30 {
		t.Fatalf("after three tasks/get: last_seen_at %.0f s old, SweepIdle closed %d (%v)", age, n, err)
	}
	if after := e.virtualRows("research"); len(after) != 1 || !bytes.Equal(after[0].hash, rows[0].hash) {
		t.Fatalf("the virtual session after the sweep: %+v", after)
	}
}

// One reading of MCP-Protocol-Version (D172): a modern request whose header is repeated is refused at the gate with
// rule 4's -32022 naming the combined value (RFC 9110 §5.3), whatever the order, and nothing reaches the upstream;
// a well-formed request afterwards forwards exactly one value.
func TestModernRepeatedVersionHeader(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/svc", false)
	result(t, m.req(1, "server/discover", nil))
	body := m.body(2, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "dup"}})
	for _, vs := range [][]string{{mcpwire.V20260728, mcpwire.V20251125}, {mcpwire.V20251125, mcpwire.V20260728}, {mcpwire.V20260728, mcpwire.V20260728}} {
		req, _ := http.NewRequest(http.MethodPost, r.url+"/mcp/svc", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for _, v := range vs {
			req.Header.Add("MCP-Protocol-Version", v)
		}
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "echo")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		rep := reply{status: resp.StatusCode, header: resp.Header, body: b}
		if rep.status != http.StatusBadRequest {
			t.Fatalf("MCP-Protocol-Version %q: %d %s", vs, rep.status, b)
		}
		code, data := rpcError(t, rep)
		if code != mcpwire.CodeUnsupportedVersion || data["requested"] != strings.Join(vs, ", ") {
			t.Fatalf("MCP-Protocol-Version %q: %s", vs, b)
		}
	}
	if n := up.Calls("echo"); n != 0 {
		t.Fatalf("the upstream received %d calls with a header the gate read differently", n)
	}
	result(t, m.req(3, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "once"}}))
	if v := lastRequest(t, up, "tools/call").Header.Values("MCP-Protocol-Version"); len(v) != 1 || v[0] != mcpwire.V20260728 {
		t.Fatalf("the upstream got MCP-Protocol-Version %q", v)
	}
}

// slowAdmission takes wait to decide (allow), and gives up when its context ends.
type slowAdmission struct {
	wait time.Duration
	n    atomic.Int32
}

func (s *slowAdmission) Admit(ctx context.Context, _ hooks.AdmitRequest) (hooks.Decision, error) {
	s.n.Add(1)
	select {
	case <-ctx.Done():
		return hooks.Decision{}, ctx.Err()
	case <-time.After(s.wait):
		return hooks.Decision{Outcome: hooks.Allow}, nil
	}
}

// The Admission session decision a principal's requests share on a replica (D177) does not end with the request
// that started it: that client leaving while Admission runs refuses nobody else, and Admission is asked once.
func TestModernAdmissionFlightOutlivesItsLeader(t *testing.T) {
	e := newEnv(t)
	_, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	slow := &slowAdmission{wait: time.Second}
	r := e.replica("a", replicaOpts{ops: []hooks.Operation{hooks.OpSession},
		withDeps: func(_ *replica, deps *Deps, _ *pg.Listener, _ *slog.Logger) { deps.Admission = slow }})
	// The era is known first (another principal's server/discover), so the requests below go straight to Admission.
	k0, _ := e.apiKey("first")
	result(t, newModern(t, k0, r.url+"/mcp/svc", false).req(0, "server/discover", nil))
	key, _ := e.apiKey("buyer")
	m := newModern(t, key, r.url+"/mcp/svc", false)
	lctx, leave := context.WithCancel(context.Background())
	defer leave()
	led := make(chan struct{})
	go func() {
		defer close(led)
		req, _ := http.NewRequestWithContext(lctx, http.MethodPost, r.url+"/mcp/svc", strings.NewReader(m.body(1, "tools/list", nil)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", mcpwire.V20260728)
		req.Header.Set("Mcp-Method", "tools/list")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	eventually(t, 5*time.Second, "the first request asks Admission", func() bool { return slow.n.Load() == 2 })
	patient := make(chan reply, 1)
	go func() { patient <- m.req(2, "tools/list", nil) }()
	time.Sleep(200 * time.Millisecond) // the patient request waits for the decision in flight
	leave()
	<-led
	rep := <-patient
	if rep.status != http.StatusOK {
		t.Fatalf("a request refused because another request of its principal left while Admission ran: %d %s", rep.status, rep.body)
	}
	result(t, rep)
	if n := slow.n.Load(); n != 2 {
		t.Fatalf("Admission asked %d times", n)
	}
}

// An allowed Admission session decision whose principal never gets a modern answer (an unknown tool) does not stay
// in the replica's flights for ever: once admitHold has passed, the next flight sweeps it.
func TestModernAdmissionFlightsSwept(t *testing.T) {
	e := newEnv(t)
	_, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	adm := &fakeAdmission{}
	var skew atomic.Int64
	r := e.replica("a", replicaOpts{admission: adm, ops: []hooks.Operation{hooks.OpSession},
		now: func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }})
	flight := func(principal string) bool {
		r.h.virtuals.mu.Lock()
		defer r.h.virtuals.mu.Unlock()
		_, ok := r.h.virtuals.flights[vkey{e.team, "svc", principal}]
		return ok
	}
	k1, p1 := e.apiKey("one")
	rep := newModern(t, k1, r.url+"/mcp/svc", false).req(1, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
	if code, _ := rpcError(t, rep); code != mcpwire.CodeInvalidParams || !flight(p1) || len(e.virtualRows("svc")) != 0 {
		t.Fatalf("an unknown tool: %s; flight %v", rep.body, flight(p1))
	}
	skew.Store(int64(admitHold + time.Second))
	k2, p2 := e.apiKey("two")
	result(t, newModern(t, k2, r.url+"/mcp/svc", false).req(1, "tools/list", nil))
	if flight(p1) || flight(p2) {
		t.Fatalf("flights after admitHold: %v %v", flight(p1), flight(p2))
	}
	if adm.n() != 2 {
		t.Fatalf("Admission asked %d times", adm.n())
	}
}

// A dispatch lost after it recorded a multi round-trip call's first upstream id (D178) and before it logged the
// round leaves an attempt with a first id, no upstream task and no round. Its re-attach can neither poll nor send the
// call afresh: it fails the task upstream_error at once, and sends the upstream nothing.
func TestModernReattachFirstIDWithoutRound(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	up, url := mrtrDouble(t)
	up.Tools = []json.RawMessage{toolWith("research", "")}
	e.service("research", url, nil)
	log := store.NewLog(e.pool)
	id, wid, _ := directModernTask(t, e, log, "research")
	fence, err := log.BeginDispatch(ctx, id, 1, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.ConsumeWorkItem(ctx, wid, id); err != nil {
		t.Fatal(err)
	}
	if err := log.SetFirstUpstreamID(ctx, fence, "q1", ""); err != nil {
		t.Fatal(err)
	}
	d2 := &Dispatcher{ReplicaID: "r2", Log: log, Registry: relay.NewRegistry(), Services: e.svcs, minPoll: 50 * time.Millisecond}
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	res, err := d2.Dispatch(dctx, lifecycle.DispatchInput{TaskID: id.String(), N: 1, WorkItemID: wid.String(), Resume: true})
	if err != nil || res.Outcome != lifecycle.DispatchTerminal || res.Reason != string(store.ReasonUpstreamError) || time.Since(start) > 5*time.Second {
		t.Fatalf("the re-attach after %v: %+v %v", time.Since(start).Round(time.Millisecond), res, err)
	}
	if row, err := log.Load(ctx, id); err != nil || row.State != store.StateFailed {
		t.Fatalf("the task: %+v %v", row, err)
	}
	if n := len(up.Requests()); n != 0 {
		t.Fatalf("the upstream received %d requests", n)
	}
}

// recordingLifecycle is a TaskLifecycle that records the cancels it is sent.
type recordingLifecycle struct {
	mu      sync.Mutex
	cancels []a2a.TaskID
}

func (l *recordingLifecycle) StartWithMessage(context.Context, lifecycle.Input, string) (lifecycle.MessageResult, error) {
	return lifecycle.MessageResult{}, nil
}

func (l *recordingLifecycle) Cancel(_ context.Context, id a2a.TaskID) (lifecycle.CancelResult, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancels = append(l.cancels, id)
	return lifecycle.CancelResult{}, true, nil
}

func (l *recordingLifecycle) SignalInputResponse(context.Context, a2a.TaskID, int64) error {
	return nil
}

func (l *recordingLifecycle) cancelled(id uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.cancels {
		if string(c) == id.String() {
			return true
		}
	}
	return false
}

// noWaker never wakes (the answers below read the log at once or not at all).
type noWaker struct{}

func (noWaker) WaitTask(context.Context, uuid.UUID) (<-chan struct{}, func()) { return nil, func() {} }

// goneCtx is a request's context once its client has closed the response stream, as AnswerCall sees it between its
// reads and its answer: Err is set while the reads (pgx watches Done only) still run.
type goneCtx struct{ context.Context }

func (goneCtx) Err() error { return context.Canceled }

// A client that closes its response stream before the answer to its modern tools/call is written has cancelled the
// request and never learned the task's id (relay §11.12 step 4, D178): a quoted task too is sent kontra.cancel and
// nothing is written, and so is one whose policy read fails because the client left; a client that stays gets its
// CreateTaskResult and cancels nothing.
func TestAnswerCallClientGone(t *testing.T) {
	e := newEnv(t)
	e.service("research", "http://127.0.0.1:1/mcp", nil)
	ctx := context.Background()
	log := store.NewLog(e.pool)
	svc, err := log.LoadService(ctx, e.team, "research")
	if err != nil {
		t.Fatal(err)
	}
	lc := &recordingLifecycle{}
	tasks, err := NewTasks(TasksConfig{Log: log, Waker: noWaker{}, Lifecycle: lc})
	if err != nil {
		t.Fatal(err)
	}
	quoted := func() uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		exp := time.Now().Add(time.Hour)
		nt, err := store.NewMCPTask(id, svc, store.Owner{BuyerID: uuid.New(), Principal: "key:x", Via: store.ViaAPIKey},
			store.StateQuoted, &exp, "https://app.example/#/t/approve", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := log.CreateTask(ctx, nil, nt); err != nil {
			t.Fatal(err)
		}
		return id
	}

	stays := quoted()
	w := httptest.NewRecorder()
	tasks.AnswerCall(ctx, w, svc, stays, json.RawMessage(`7`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"resultType":"task"`) {
		t.Fatalf("a client that stays: %d %s", w.Code, w.Body)
	}

	gone := quoted()
	w = httptest.NewRecorder()
	tasks.AnswerCall(goneCtx{ctx}, w, svc, gone, json.RawMessage(`8`))
	if w.Body.Len() != 0 {
		t.Fatalf("an answer was written to a client that left: %d %s", w.Code, w.Body)
	}
	eventually(t, 5*time.Second, "the quoted task is sent kontra.cancel", func() bool { return lc.cancelled(gone) })

	failing := quoted()
	other := svc
	other.Name, other.Retention = "no-such-service", 0 // the policy read fails
	w = httptest.NewRecorder()
	tasks.AnswerCall(goneCtx{ctx}, w, other, failing, json.RawMessage(`9`))
	if w.Body.Len() != 0 {
		t.Fatalf("an answer was written to a client that left during the policy read: %d %s", w.Code, w.Body)
	}
	eventually(t, 5*time.Second, "the task is sent kontra.cancel", func() bool { return lc.cancelled(failing) })
	if lc.cancelled(stays) {
		t.Fatal("the task of the client that stayed was cancelled")
	}
}
