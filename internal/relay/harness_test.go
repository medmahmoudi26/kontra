package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

var (
	buyerA = store.Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000a"), Principal: "key:kk_a_00000001", Via: store.ViaAPIKey}
	buyerB = store.Owner{BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000b"), Principal: "key:kk_b_00000001", Via: store.ViaAPIKey}
)

const replica = "replica-test-1"

// staticBackends answers one backend and card info for every service, or, when by is set, what by
// answers for the attempt (the tunnel tests' sessions).
type staticBackends struct {
	mu   sync.Mutex
	b    upstream.Backend
	info upstream.Info
	err  error
	by   Backends
}

func (s *staticBackends) Backend(ctx context.Context, svc store.ServiceRef, a *store.Attempt) (upstream.Backend, upstream.Info, error) {
	s.mu.Lock()
	by := s.by
	b, info, err := s.b, s.info, s.err
	s.mu.Unlock()
	if by != nil {
		return by.Backend(ctx, svc, a)
	}
	return b, info, err
}

func (s *staticBackends) use(by Backends) {
	s.mu.Lock()
	s.by = by
	s.mu.Unlock()
}

func (s *staticBackends) set(b upstream.Backend, info upstream.Info, err error) {
	s.mu.Lock()
	s.b, s.info, s.err = b, info, err
	s.mu.Unlock()
}

// signalRecorder records kontra.dispatched signals.
type signalRecorder struct {
	mu  sync.Mutex
	got []string
}

func (s *signalRecorder) SignalDispatched(_ context.Context, taskID a2a.TaskID, n int) error {
	s.mu.Lock()
	s.got = append(s.got, string(taskID)+"#"+string(rune('0'+n)))
	s.mu.Unlock()
	return nil
}

func (s *signalRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// testHeartbeater stands in for lifecycle.Heartbeater, which records Temporal heartbeats (WP-05).
type testHeartbeater struct{}

func (testHeartbeater) HeartbeatInterval() time.Duration    { return 5 * time.Second }
func (testHeartbeater) Heartbeat(ctx context.Context) error { return ctx.Err() }

// rebuild stands in for lifecycle.Rebuild (WP-05): the stored request and call context as a2a-go's payload.
func rebuild(taskID a2a.TaskID, wi *store.WorkItem) (*workqueue.Payload, error) {
	return &workqueue.Payload{Type: workqueue.PayloadTypeExecute, TaskID: taskID, ExecuteRequest: wi.Payload.Request, CallContext: wi.Payload.CallContext}, nil
}

// env is one migrated database with the relay around it.
type env struct {
	t        *testing.T
	dsn      string
	pool     *pgxpool.Pool
	log      *store.Log
	svc      store.ServiceRef
	reg      *Registry
	handlers *Handlers
	disp     *Dispatcher
	backends *staticBackends
	signals  *signalRecorder
	queue    *fakeQueue
	intc     *Interceptors
	render   *blob.Renderer
}

type envOptions struct {
	push     a2apush.ConfigStore
	opts     ExecutorOptions
	canceler Canceller
	reviews  ReviewMessenger
	admit    hooks.Admission
	rules    hooks.KeyRules
	policy   ssrf.Policy
	// writeTimeout replaces the log's write timeout (store.WriteTimeout) when set.
	writeTimeout time.Duration
	// wrapTaskStore wraps each service's task store beneath the dispatch's store-failure notes.
	wrapTaskStore func(*store.Store) taskstore.Store
	// bucket is the claim-check bucket (blobs:), nil without one.
	bucket blob.Store
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	dsn := pgtest.Migrated(t)
	e := &env{t: t, dsn: dsn, pool: pgtest.Pool(t, dsn), reg: NewRegistry(), backends: &staticBackends{}, signals: &signalRecorder{}}
	var logOpts []store.LogOption
	if o.writeTimeout > 0 {
		logOpts = append(logOpts, store.WithWriteTimeout(o.writeTimeout))
	}
	e.log = store.NewLog(e.pool, logOpts...)
	tenant := uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`, tenant)
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source, retries) values ($1, 'hello', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config', 2)`, tenant)
	e.svc = store.ServiceRef{TenantID: tenant, Tenant: "default", Name: "hello", Kind: "a2a", Timeout: time.Hour, Retries: 2, Retention: 7 * 24 * time.Hour}
	listener := runListener(t, dsn)
	if o.opts.PollInterval == 0 {
		o.opts.PollInterval = 50 * time.Millisecond
	}
	e.queue = &fakeQueue{e: e, done: make(chan dispatchDone, 64)}
	e.render = blob.NewRenderer(o.bucket, e.pool, nil)
	o.opts.Bucket, o.opts.Render = o.bucket, e.render
	e.intc = &Interceptors{Svc: e.svc, Log: e.log, Rules: o.rules, Admit: o.admit, Canceler: o.canceler, Reviews: o.reviews,
		PushPolicy: o.policy, Bucket: o.bucket, Render: e.render}
	e.handlers = NewHandlers(HandlerDeps{
		Log: e.log, Puller: store.NewPullerProvider(e.log, listener), PushStore: o.push,
		Queue:        func(store.ServiceRef) workqueue.Queue { q, _ := workqueue.NewPushQueue(e.queue); return q },
		Interceptors: func(store.ServiceRef) []a2asrv.CallInterceptor { return e.intc.Chain() },
		Registry:     e.reg, Backends: e.backends, Signals: e.signals, Executor: o.opts,
		wrapTaskStore: o.wrapTaskStore,
	})
	e.disp = &Dispatcher{ReplicaID: replica, Log: e.log, Registry: e.reg, Handlers: e.handlers, Backends: e.backends,
		Rebuild: rebuild, Heartbeater: testHeartbeater{}}
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func runListener(t *testing.T, dsn string) *pg.Listener {
	t.Helper()
	cfg, err := pg.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	l := pg.NewListener(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(10 * time.Second)
	for !l.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("the listener did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return l
}

// callContext is the call context a2a-go's codec encodes, as the work queue stores it (sanitized: only
// A2A-Version and A2A-Extensions of the request's headers), plus extra headers a test adds.
func callContext(o store.Owner, extra map[string][]string) map[string]any {
	params := map[string]any{"a2a-version": []any{"1.0"}}
	for k, v := range extra {
		vals := []any{}
		for _, s := range v {
			vals = append(vals, s)
		}
		params[strings.ToLower(k)] = vals
	}
	return map[string]any{"callContext": map[string]any{
		"svcParams": params,
		"user": map[string]any{"name": o.BuyerID.String(), "authenticated": true,
			"attributes": map[string]any{AttrPrincipal: o.Principal, AttrVia: o.Via}},
		"protocolVersion": "1.0",
	}}
}

type taskSpec struct {
	owner     store.Owner
	contextID string // the caller's; empty mints one
	text      string
	refs      []a2a.TaskID
	headers   map[string][]string // extra service params in the stored call context
	config    *a2a.SendMessageConfig
	metadata  map[string]any
	session   *uuid.UUID // attempt 1 on this tunnel session (key k1_…, replica the test's), direct when nil
}

// newTask pre-creates a gate task as the work queue does: the task at seq 1 (submitted), its work item,
// and attempt 1 on the direct backend. It returns the task id and the work item id.
func (e *env) newTask(s taskSpec) (a2a.TaskID, uuid.UUID) {
	e.t.Helper()
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7())
	ctxID := s.contextID
	if ctxID == "" {
		ctxID = uuid.Must(uuid.NewV7()).String()
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(s.text))
	msg.ContextID = s.contextID
	msg.ReferenceTasks = s.refs
	hist := *msg
	hist.TaskID, hist.ContextID = a2a.TaskID(id.String()), ctxID
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: ctxID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{&hist}}
	wid := uuid.Must(uuid.NewV7())
	req := &a2a.SendMessageRequest{Message: msg, Config: s.config, Metadata: s.metadata}
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := e.log.CreateTask(ctx, tx, store.NewTask{ID: id, Service: e.svc, Protocol: store.ProtocolA2A, Owner: s.owner,
			ContextID: ctxID, State: store.StateSubmitted, Snapshot: snap}); err != nil {
			return err
		}
		return e.log.InsertWorkItem(ctx, tx, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute,
			Payload: store.WorkPayload{Request: req, CallContext: callContext(s.owner, s.headers), Owner: s.owner}})
	})
	if err != nil {
		e.t.Fatal(err)
	}
	att := store.Attempt{TaskID: id, N: 1, Backend: store.BackendDirect}
	if s.session != nil {
		att = store.Attempt{TaskID: id, N: 1, Backend: store.BackendTunnel, TunnelSession: s.session, KeyID: testKeyID, ReplicaID: replica}
	}
	if err := e.log.BeginAttempt(ctx, att); err != nil {
		e.t.Fatal(err)
	}
	return a2a.TaskID(id.String()), wid
}

// testKeyID is the node key of the tunnel attempts the tests write.
const testKeyID = "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa"

// followUp stores a follow-up turn's work item, as kontra.message's Write does.
func (e *env) followUp(taskID a2a.TaskID, o store.Owner, text string) uuid.UUID {
	e.t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID = taskID
	wid := uuid.Must(uuid.NewV7())
	if err := e.log.InsertWorkItem(context.Background(), nil, store.WorkItem{ID: wid, TaskID: uid(e.t, taskID), Kind: store.WorkExecute,
		Payload: store.WorkPayload{Request: &a2a.SendMessageRequest{Message: msg}, CallContext: callContext(o, nil), Owner: o}}); err != nil {
		e.t.Fatal(err)
	}
	return wid
}

func (e *env) dispatch(ctx context.Context, taskID a2a.TaskID, n int, wid uuid.UUID, resume bool) lifecycle.DispatchResult {
	e.t.Helper()
	res, err := e.disp.Dispatch(ctx, lifecycle.DispatchInput{TaskID: string(taskID), N: n, WorkItemID: wid.String(), Resume: resume})
	if err != nil {
		e.t.Fatalf("dispatch: %v", err)
	}
	return res
}

func uid(t testing.TB, id a2a.TaskID) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(string(id))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *env) row(id a2a.TaskID) *store.TaskRow {
	e.t.Helper()
	r, err := e.log.Load(context.Background(), uid(e.t, id))
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) attempt(id a2a.TaskID, n int) *store.Attempt {
	e.t.Helper()
	a, err := e.log.GetAttempt(context.Background(), uid(e.t, id), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

// events returns the task's log: kinds and raw payloads.
func (e *env) events(id a2a.TaskID) []store.Event {
	e.t.Helper()
	evs, err := e.log.Events(context.Background(), uid(e.t, id), 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

// logText is the task's whole log and projection as text, for leak checks.
func (e *env) logText(id a2a.TaskID) string {
	var b strings.Builder
	for _, ev := range e.events(id) {
		b.Write(ev.Payload)
		b.WriteByte('\n')
	}
	snap, _ := json.Marshal(e.row(id).Snapshot)
	b.Write(snap)
	return b.String()
}

func (e *env) states(id a2a.TaskID) []string {
	var out []string
	for _, ev := range e.events(id) {
		var sr a2a.StreamResponse
		if err := json.Unmarshal(ev.Payload, &sr); err != nil {
			e.t.Fatalf("seq %d: %v", ev.Seq, err)
		}
		switch v := sr.Event.(type) {
		case *a2a.Task:
			out = append(out, "task:"+string(v.Status.State))
		case *a2a.TaskStatusUpdateEvent:
			out = append(out, "status:"+string(v.Status.State))
		case *a2a.TaskArtifactUpdateEvent:
			out = append(out, "artifact")
		case *a2a.Message:
			out = append(out, "message")
		}
	}
	return out
}

// fakeQueue is the probes' work queue for the frontend tests (what lifecycle.Queue does over Temporal):
// Write pre-creates a new task with its work item and attempt 1, or stores a follow-up's work item, and
// runs the dispatch in a goroutine with the stored work item, as the dispatch activity will.
type fakeQueue struct {
	e    *env
	mu   sync.Mutex
	ids  []a2a.TaskID
	done chan dispatchDone
	hold bool // record writes without dispatching
	sync bool // dispatch before Write returns: the turn ends before a2a-go's subscription reads the store
}

type dispatchDone struct {
	task a2a.TaskID
	res  lifecycle.DispatchResult
	err  error
}

func (q *fakeQueue) Write(ctx context.Context, p *workqueue.Payload) (a2a.TaskID, error) {
	e := q.e
	o, err := CallerOwner(ctx)
	if err != nil {
		return "", err
	}
	cc := sanitizeCallContext(p.CallContext)
	req := p.ExecuteRequest
	wid := uuid.Must(uuid.NewV7())
	n := 1
	var id uuid.UUID
	if req.Message.TaskID == "" {
		id, err = uuid.Parse(string(p.TaskID))
		if err != nil {
			return "", err
		}
		if adm, ok := hooks.AdmittedFrom(ctx); ok {
			id = uuid.MustParse(adm.TaskID)
		}
		msg := *req.Message
		ctxID := msg.ContextID
		if ctxID == "" {
			ctxID = uuid.Must(uuid.NewV7()).String()
		}
		msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), ctxID
		snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: ctxID, Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{&msg}}
		err = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			if _, err := e.log.CreateTask(ctx, tx, store.NewTask{ID: id, Service: e.svc, Protocol: store.ProtocolA2A, Owner: o,
				ContextID: ctxID, State: store.StateSubmitted, Snapshot: snap}); err != nil {
				return err
			}
			return e.log.InsertWorkItem(ctx, tx, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute,
				Payload: store.WorkPayload{Request: req, CallContext: cc, Owner: o}})
		})
		if err != nil {
			return "", err
		}
		if err := e.log.BeginAttempt(ctx, store.Attempt{TaskID: id, N: 1, Backend: store.BackendDirect}); err != nil {
			return "", err
		}
		p.TaskID = a2a.TaskID(id.String())
	} else {
		id = uid(e.t, req.Message.TaskID)
		if err := e.log.InsertWorkItem(ctx, nil, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute,
			Payload: store.WorkPayload{Request: req, CallContext: cc, Owner: o}}); err != nil {
			return "", err
		}
		a, err := e.log.LatestAttempt(ctx, id)
		if err != nil {
			return "", err
		}
		n = a.N
	}
	q.mu.Lock()
	q.ids = append(q.ids, p.TaskID)
	hold, sync := q.hold, q.sync
	q.mu.Unlock()
	run := func() {
		res, err := e.disp.Dispatch(context.Background(), lifecycle.DispatchInput{TaskID: id.String(), N: n, WorkItemID: wid.String()})
		q.done <- dispatchDone{task: p.TaskID, res: res, err: err}
	}
	switch {
	case hold:
	case sync:
		run()
	default:
		go run()
	}
	return p.TaskID, nil
}

func (q *fakeQueue) written() []a2a.TaskID {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]a2a.TaskID(nil), q.ids...)
}

func (q *fakeQueue) wait(t *testing.T) dispatchDone {
	t.Helper()
	select {
	case d := <-q.done:
		if d.err != nil {
			t.Fatalf("dispatch of %s: %v", d.task, d.err)
		}
		return d
	case <-time.After(20 * time.Second):
		t.Fatal("no dispatch finished")
	}
	return dispatchDone{}
}

// sanitizeCallContext keeps only A2A-Version and A2A-Extensions of the request's headers (spec §8.1).
func sanitizeCallContext(cc map[string]any) map[string]any {
	raw, _ := json.Marshal(cc)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if c, ok := out["callContext"].(map[string]any); ok {
		if sp, ok := c["svcParams"].(map[string]any); ok {
			for k := range sp {
				if k != "a2a-version" && k != "a2a-extensions" {
					delete(sp, k)
				}
			}
		}
	}
	return out
}

// principal is the edge's authenticated caller for buyer o.
func principal(o store.Owner) auth.Principal {
	return auth.Principal{Kind: auth.KindKey, ID: o.Principal, BuyerID: o.BuyerID, Via: auth.Via(o.Via), Scopes: []auth.Scope{auth.ScopeHire}}
}

func asCaller(o store.Owner) context.Context {
	return auth.WithPrincipal(context.Background(), principal(o))
}

// upstreamAgent is an in-process a2a-go v2.7.0 seller (local mode) behind a recording HTTP handler.
type upstreamAgent struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []recorded
	sends    atomic.Int64
}

type recorded struct {
	method string
	header http.Header
	body   []byte
}

type agentConfig struct {
	binding   a2a.TransportProtocol // JSONRPC (default) or HTTP+JSON
	streaming bool                  // the card's capabilities.streaming
	refuse    bool                  // refuse SendStreamingMessage (a2a-cli exec mode without --chunk, D15)
}

func newAgent(t *testing.T, exec a2asrv.AgentExecutor, cfg agentConfig) (*upstreamAgent, upstream.Backend, upstream.Info) {
	t.Helper()
	if cfg.binding == "" {
		cfg.binding = a2a.TransportProtocolJSONRPC
	}
	var opts []a2asrv.RequestHandlerOption
	if cfg.refuse {
		enf, err := a2asrv.NewAgentCardEnforcer(&a2a.AgentCard{Capabilities: a2a.AgentCapabilities{Streaming: false}})
		if err != nil {
			t.Fatal(err)
		}
		opts = append(opts, a2asrv.WithCallInterceptors(enf))
	}
	h := a2asrv.NewHandler(exec, opts...)
	var inner http.Handler
	if cfg.binding == a2a.TransportProtocolHTTPJSON {
		inner = a2asrv.NewRESTHandler(h)
	} else {
		inner = a2asrv.NewJSONRPCHandler(h)
	}
	a := &upstreamAgent{}
	mux := http.NewServeMux()
	mux.Handle("/a2a/", http.StripPrefix("/a2a", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)
		method := rpc.Method
		if cfg.binding == a2a.TransportProtocolHTTPJSON {
			method = r.Method + " " + r.URL.Path
		}
		if strings.Contains(method, "SendMessage") || strings.Contains(method, "SendStreamingMessage") ||
			strings.HasSuffix(method, "message:send") || strings.HasSuffix(method, "message:stream") {
			a.sends.Add(1)
		}
		a.mu.Lock()
		a.requests = append(a.requests, recorded{method: method, header: r.Header.Clone(), body: body})
		a.mu.Unlock()
		inner.ServeHTTP(w, r)
	})))
	a.srv = httptest.NewServer(mux)
	t.Cleanup(a.srv.Close)
	b, err := upstream.NewDirect(a.srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	return a, b, upstream.Info{ProtocolVersion: "1.0", Binding: cfg.binding, Path: "/a2a/", Streaming: cfg.streaming}
}

func (a *upstreamAgent) recorded() []recorded {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recorded(nil), a.requests...)
}

// sentMessages are the messages the upstream received in SendMessage and SendStreamingMessage calls.
func (a *upstreamAgent) sentMessages(t *testing.T) []*a2a.Message {
	t.Helper()
	var out []*a2a.Message
	for _, r := range a.recorded() {
		if !strings.Contains(r.method, "SendMessage") && !strings.Contains(r.method, "SendStreamingMessage") {
			continue
		}
		var rpc struct {
			Params a2a.SendMessageRequest `json:"params"`
		}
		if err := json.Unmarshal(r.body, &rpc); err != nil {
			t.Fatal(err)
		}
		out = append(out, rpc.Params.Message)
	}
	return out
}
