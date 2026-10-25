package a2ahttp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/auth"
	"github.com/medmahmoudi26/kontra/internal/card"
	"github.com/medmahmoudi26/kontra/internal/edge"
	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/pg"
	"github.com/medmahmoudi26/kontra/internal/pgtest"
	"github.com/medmahmoudi26/kontra/internal/relay"
	"github.com/medmahmoudi26/kontra/internal/sse"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/temporaltest"
	"github.com/medmahmoudi26/kontra/internal/upstream"
)

// TestMain stops the Temporal development server the real-stack test started.
func TestMain(m *testing.M) {
	code := m.Run()
	temporaltest.Stop()
	os.Exit(code)
}

// seller is an a2a-go v2.7.0 agent (local mode) that asks a question on the first turn and, on a
// follow-up, works until released, then answers with an artifact and completes.
type seller struct {
	release chan struct{}
	sends   atomic.Int64
	srv     *httptest.Server
}

func (s *seller) executor() a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if ec.StoredTask == nil {
				if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
					return
				}
				yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))), nil)
				return
			}
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
			select {
			case <-s.release:
			case <-ctx.Done():
				return
			}
			if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("booked: "+ec.Message.Parts[0].Text())), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done"))), nil)
		}
	})
}

func newSeller(t *testing.T) *seller {
	s := &seller{release: make(chan struct{})}
	h := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(s.executor()))
	mux := http.NewServeMux()
	mux.Handle("/a2a/", http.StripPrefix("/a2a", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"SendMessage"`) || strings.Contains(string(body), `"SendStreamingMessage"`) {
			s.sends.Add(1)
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		h.ServeHTTP(w, r)
	})))
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

const sellerCard = `{"name":"booking","description":"books things","version":"1.0.0",
"supportedInterfaces":[{"url":"http://seller.invalid/a2a/","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":{"streaming":true,"extensions":[{"uri":"https://example.com/ext/x/v1"}]},
"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[{"id":"book","name":"book","description":"book","tags":["b"]}]}`

type backends struct {
	b    upstream.Backend
	info upstream.Info
}

func (b backends) Backend(context.Context, store.ServiceRef) (upstream.Backend, upstream.Info, error) {
	return b.b, b.info, nil
}

type directOnly struct{}

func (directOnly) BackendFor(context.Context, uuid.UUID, string) (store.Backend, error) {
	return store.BackendDirect, nil
}

func stackListener(t *testing.T, dsn string) *pg.Listener {
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

// sseEvent is one event as a buyer reads it; id 0 is an event without an id line.
type sseEvent struct {
	id   int64
	data string
}

// buyerStream is one SSE response read by a buyer that keeps ids (a2a-go's client ignores them).
type buyerStream struct {
	resp   *http.Response
	events chan sseEvent
}

func openStream(t *testing.T, req *http.Request) (*buyerStream, *http.Response) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		return nil, resp
	}
	s := &buyerStream{resp: resp, events: make(chan sseEvent, 64)}
	go func() {
		defer close(s.events)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "" && cur.data != "":
				s.events <- cur
				cur = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				cur.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			case strings.HasPrefix(line, "data: "):
				if cur.data != "" {
					t.Errorf("an event with two data lines")
				}
				cur.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return s, resp
}

func (s *buyerStream) all(t *testing.T) []sseEvent {
	t.Helper()
	var out []sseEvent
	timeout := time.After(60 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("the stream did not end; got %+v", out)
		}
	}
}

// eventOf decodes a data line of either binding into its event.
func eventOf(t *testing.T, data string) a2a.Event {
	t.Helper()
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
	}
	raw := []byte(data)
	if json.Unmarshal(raw, &env) == nil && env.JSONRPC == "2.0" {
		raw = env.Result
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return sr.Event
}

func stateOfEvent(ev a2a.Event) a2a.TaskState {
	switch v := ev.(type) {
	case *a2a.Task:
		return v.Status.State
	case *a2a.TaskStatusUpdateEvent:
		return v.Status.State
	}
	return ""
}

// carol is another buyer of alice's tenant: the service admits her, alice's tasks are not hers.
var carol = auth.Principal{Kind: auth.KindKey, ID: "key:kk_carol_00000001", BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000c"),
	Via: auth.ViaAPIKey, Scopes: []auth.Scope{auth.ScopeHire}, TenantID: &tenantID, TenantSlug: "default"}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// stack is the gate's A2A surface over the real relay: a2a-go's handler with the gate's interceptors, the
// Temporal-backed work queue, TaskLifecycle on a development server, the dispatch activity, the SSE
// writer on the replica's LISTEN connection, behind the edge.
type stack struct {
	url    string
	pool   *pgxpool.Pool
	log    *store.Log
	seller *seller
}

func newStack(t *testing.T) *stack {
	t.Helper()
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	log := store.NewLog(pool)
	tenant := uuid.Must(uuid.NewV7())
	ctx := context.Background()
	for _, q := range []string{
		`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`,
		`insert into kontra.services (tenant_id, name, kind, backend, source, retries) values ($1, 'hello', 'a2a', '{"a2a": {"url": "http://127.0.0.1:1"}}', 'config', 2)`,
	} {
		if _, err := pool.Exec(ctx, q, tenant); err != nil {
			t.Fatal(err)
		}
	}
	svc := store.ServiceRef{TenantID: tenant, Tenant: "default", Name: "hello", Kind: "a2a", Timeout: time.Hour, Retries: 2, Retention: 7 * 24 * time.Hour}
	listener := stackListener(t, dsn)
	c := temporaltest.Client(t)
	lc := lifecycle.NewClient(c)

	sl := newSeller(t)
	b, err := upstream.NewDirect(sl.srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	bk := backends{b: b, info: upstream.Info{ProtocolVersion: "1.0", Binding: a2a.TransportProtocolJSONRPC, Path: "/a2a/", Streaming: true}}
	reg := relay.NewRegistry()
	intc := &relay.Interceptors{Svc: svc, Log: log, Canceler: lc, Reviews: lc}
	hs := relay.NewHandlers(relay.HandlerDeps{
		Log: log, Puller: store.NewPullerProvider(log, listener),
		Queue: func(s store.ServiceRef) workqueue.Queue {
			return lifecycle.NewQueue(s, log, lc, lifecycle.QueueOptions{Owner: relay.CallerOwner, Hooks: lifecycle.HooksSnapshot{LifecycleQueue: lifecycle.QueueLifecycle}})
		},
		Interceptors: func(store.ServiceRef) []a2asrv.CallInterceptor { return intc.Chain() },
		Registry:     reg, Backends: bk, Signals: lc, Executor: relay.ExecutorOptions{PollInterval: 50 * time.Millisecond},
	})
	disp := &relay.Dispatcher{ReplicaID: "replica-a2ahttp", Log: log, Registry: reg, Handlers: hs, Backends: bk}
	w, err := lifecycle.NewWorkers(c, lifecycle.WorkerConfig{ReplicaID: "replica-a2ahttp", DrainTimeout: time.Second},
		lifecycle.Activities{Lifecycle: &lifecycle.LifecycleActivities{Log: log, Backends: directOnly{}}, Dispatch: disp})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)

	var front http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { front.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	body, err := card.Rewrite(card.Input{Upstream: []byte(sellerCard), PublicURL: srv.URL, ServicePath: "/agents/hello"})
	if err != nil {
		t.Fatal(err)
	}
	served := &card.Served{Body: body, Upstream: []byte(sellerCard), ETag: card.ETag(body), LastModified: time.Now()}
	allowList := allow(t, "org:default")
	a := New(Deps{
		Services: registry{"default/hello": &Service{Ref: svc, Allow: allowList, Cards: func() (*card.Served, error) { return served, nil }}},
		Handlers: hs, SSE: sse.NewWriterWithOptions(log, listener, sse.Options{KeepAlive: 200 * time.Millisecond}),
		Auth: authn{"alice": alice, "bob": bob, "carol": carol},
	})
	rt := edge.NewRouter()
	a.Register(rt)
	front = edge.Middleware(edge.MiddlewareConfig{}, rt)
	return &stack{url: srv.URL, pool: pool, log: log, seller: sl}
}

func (s *stack) request(t *testing.T, method, path, body, token string, header ...string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, s.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("A2A-Version", "1.0")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	return r
}

func (s *stack) carried(t *testing.T, id a2a.TaskID) []int64 {
	t.Helper()
	evs, err := s.log.Events(context.Background(), uuid.MustParse(string(id)), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, ev := range evs {
		if ev.Kind.Carried() {
			out = append(out, ev.Seq)
		}
	}
	return out
}

// TestStreamsOverTheRealStack runs the gate's A2A surface over the real relay and lifecycle (the audit's
// "follow-up turn through the real stack", spec §7, §9, §14.1):
//
//   - a2a-go's resolver fetches GET {svc} as the card, and a2a-go's client sends through the gate;
//   - SendStreamingMessage of a new task on JSON-RPC: the creation row with id: 1, closing at input-required;
//   - two concurrent follow-ups: exactly one is accepted, the other refused before any SSE header;
//   - the accepted follow-up's stream leads with the snapshot at V without an id and closes after completed;
//   - SubscribeToTask resumes over HTTP+JSON with Last-Event-ID, and refuses another buyer.
func TestStreamsOverTheRealStack(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	// D10: `a2a send -a http://host/agents/hello` resolves the card at the service URL itself.
	authed := &http.Client{Transport: bearer{"alice"}}
	resolved, err := agentcard.NewResolver(authed).Resolve(ctx, s.url+"/agents/hello")
	if err != nil {
		t.Fatalf("resolve GET {svc}: %v", err)
	}
	if len(resolved.SupportedInterfaces) != 2 || resolved.SupportedInterfaces[0].URL != s.url+"/agents/hello" || !resolved.Capabilities.Streaming {
		t.Fatalf("resolved card %+v", resolved.SupportedInterfaces)
	}
	client, err := a2aclient.NewFromCard(ctx, resolved, a2aclient.WithJSONRPCTransport(authed))
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("a table"))})
	if err != nil {
		t.Fatalf("a2a-go client through the gate: %v", err)
	}
	if tk, ok := res.(*a2a.Task); !ok || tk.Status.State != a2a.TaskStateInputRequired || tk.Status.Message.Parts[0].Text() != "which one?" {
		t.Fatalf("a2a-go client got %+v", res)
	}

	// Turn 1: SendStreamingMessage on JSON-RPC, a new task.
	body := rpc("SendStreamingMessage", `{"message":{"messageId":"`+uuid.NewString()+`","role":"ROLE_USER","parts":[{"text":"a room"}]}}`, "turn-1")
	st, resp := openStream(t, s.request(t, "POST", "/agents/hello", body, "alice"))
	if st == nil {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("turn 1: %d %s", resp.StatusCode, b)
	}
	turn1 := st.all(t)
	if len(turn1) < 2 || turn1[0].id != 1 {
		t.Fatalf("turn 1: %+v", turn1)
	}
	first, ok := eventOf(t, turn1[0].data).(*a2a.Task)
	if !ok || first.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("turn 1 starts with %s", turn1[0].data)
	}
	taskID := first.ID
	for i, ev := range turn1 {
		if i > 0 && ev.id <= turn1[i-1].id {
			t.Fatalf("ids not increasing: %+v", turn1)
		}
		if strings.Contains(ev.data, s.seller.srv.URL) {
			t.Errorf("an event names the upstream")
		}
	}
	if last := eventOf(t, turn1[len(turn1)-1].data); stateOfEvent(last) != a2a.TaskStateInputRequired {
		t.Fatalf("turn 1 ends with %s", turn1[len(turn1)-1].data)
	}

	// Turn 2: two follow-ups at once, one per binding. Exactly one is accepted.
	follow := func(text string) string {
		return `{"message":{"messageId":"` + uuid.NewString() + `","role":"ROLE_USER","taskId":"` + string(taskID) + `","parts":[{"text":"` + text + `"}]}}`
	}
	type outcome struct {
		stream *buyerStream
		status int
		body   string
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, r := range []*http.Request{
		s.request(t, "POST", "/agents/hello/message:stream", follow("blue"), "alice"),
		s.request(t, "POST", "/agents/hello/", rpc("SendStreamingMessage", follow("red"), "turn-2"), "alice"),
	} {
		wg.Add(1)
		go func(r *http.Request) {
			defer wg.Done()
			st, resp := openStream(t, r)
			if st != nil {
				results <- outcome{stream: st}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			results <- outcome{status: resp.StatusCode, body: string(b)}
		}(r)
	}
	wg.Wait()
	close(results)
	var accepted *buyerStream
	var refusals []outcome
	for o := range results {
		if o.stream != nil {
			if accepted != nil {
				t.Fatal("both concurrent follow-ups were accepted")
			}
			accepted = o.stream
			continue
		}
		refusals = append(refusals, o)
	}
	if accepted == nil || len(refusals) != 1 {
		t.Fatalf("follow-ups: accepted %v, refused %+v", accepted != nil, refusals)
	}
	if r := refusals[0]; !strings.Contains(r.body, "UNSUPPORTED_OPERATION") && !strings.Contains(r.body, `"code":-32004`) {
		t.Errorf("the refused follow-up: %d %s", r.status, r.body)
	}
	close(s.seller.release)
	turn2 := accepted.all(t)
	if len(turn2) < 3 || turn2[0].id != 0 {
		t.Fatalf("turn 2: %+v", turn2)
	}
	lead, ok := eventOf(t, turn2[0].data).(*a2a.Task)
	if !ok || lead.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("turn 2 leads with %s", turn2[0].data)
	}
	end := turn2[len(turn2)-1]
	if fin, ok := eventOf(t, end.data).(*a2a.TaskStatusUpdateEvent); !ok || fin.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("turn 2 ends with %s", end.data)
	}
	carried := s.carried(t, taskID)
	var ids []int64
	for _, ev := range turn1 {
		ids = append(ids, ev.id)
	}
	for _, ev := range turn2[1:] {
		if ev.id <= ids[len(ids)-1] {
			t.Fatalf("turn 2 id %d after %v", ev.id, ids)
		}
		ids = append(ids, ev.id)
	}
	if fmt.Sprint(ids) != fmt.Sprint(carried) {
		t.Errorf("both turns' ids %v, the log's carried seqs %v", ids, carried)
	}
	if n := s.seller.sends.Load(); n != 3 {
		t.Errorf("the seller received %d messages: the a2a-go client's, turn 1 and one follow-up", n)
	}

	// Resume over HTTP+JSON with Last-Event-ID: K = terminal - 1 sends the snapshot as of it and the
	// terminal row, GET and POST alike; K = terminal is refused; another buyer gets TaskNotFound.
	terminalSeq := carried[len(carried)-1]
	for _, m := range []string{"GET", "POST"} {
		st, resp := openStream(t, s.request(t, m, "/agents/hello/tasks/"+string(taskID)+":subscribe", "", "alice", "Last-Event-ID", strconv.FormatInt(terminalSeq-1, 10)))
		if st == nil {
			t.Fatalf("%s resume: %d", m, resp.StatusCode)
		}
		got := st.all(t)
		if len(got) != 2 || got[0].id != 0 || got[1].id != terminalSeq {
			t.Errorf("%s resume at terminal-1: %+v", m, got)
		}
	}
	for _, c := range []struct {
		token, k, want string
		status         int
	}{
		{"alice", strconv.FormatInt(terminalSeq, 10), "UNSUPPORTED_OPERATION", 400},
		{"alice", "", "UNSUPPORTED_OPERATION", 400},
		{"alice", "999", "INVALID_PARAMS", 400},
		{"carol", "2", "TASK_NOT_FOUND", 404}, // another buyer the service admits (cross_buyer)
		{"bob", "2", "unknown service", 404},  // a caller the service does not admit (D50)
	} {
		h := []string{}
		if c.k != "" {
			h = []string{"Last-Event-ID", c.k}
		}
		st, resp := openStream(t, s.request(t, "POST", "/agents/hello/tasks/"+string(taskID)+":subscribe", "", c.token, h...))
		if st != nil {
			t.Fatalf("%s K=%q: a stream", c.token, c.k)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || !strings.Contains(string(b), c.want) {
			t.Errorf("%s K=%q: %d %s", c.token, c.k, resp.StatusCode, b)
		}
	}
	// GetExtendedAgentCard is UnsupportedOperationError on both bindings (spec §3: extendedAgentCard false).
	resp, err = http.DefaultClient.Do(s.request(t, "GET", "/agents/hello/extendedAgentCard", "", "alice"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "UNSUPPORTED_OPERATION") {
		t.Errorf("extendedAgentCard: %d %s", resp.StatusCode, b)
	}
	resp, err = http.DefaultClient.Do(s.request(t, "POST", "/agents/hello", rpc("GetExtendedAgentCard", `{}`, 1), "alice"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"code":-32004`) {
		t.Errorf("GetExtendedAgentCard: %d %s", resp.StatusCode, b)
	}
	// The same task through a2a-go's GetTask on the gate's REST route.
	resp, err = http.DefaultClient.Do(s.request(t, "GET", "/agents/hello/tasks/"+string(taskID), "", "alice"))
	if err != nil {
		t.Fatal(err)
	}
	var got a2a.Task
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.Status.State != a2a.TaskStateCompleted {
		t.Errorf("GetTask: %d %+v %v", resp.StatusCode, got.Status, err)
	}
	resp.Body.Close()
}
