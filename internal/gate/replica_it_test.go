package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/migrate"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/temporaltest"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/version"
)

// seller is an a2a-go v2.7.0 agent behind a base path: "hi" completes at once with Hello World; any other
// text works until the test releases it (or the gate cancels it), then completes with an artifact.
type seller struct {
	mu       sync.Mutex
	gates    map[string]chan struct{}
	cancels  int
	methods  []string
	srv      *httptest.Server
	cardJSON string
}

func newSeller(t *testing.T) *seller {
	t.Helper()
	s := &seller{gates: map[string]chan struct{}{}}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(s))
	mux := http.NewServeMux()
	mux.HandleFunc("/base/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, s.cardJSON)
	})
	mux.HandleFunc("/base/rpc", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, m := range []string{"SendStreamingMessage", "SendMessage", "CancelTask", "GetTask", "SubscribeToTask"} {
			if bytes.Contains(body, []byte(`"method":"`+m+`"`)) {
				s.mu.Lock()
				s.methods = append(s.methods, m)
				s.mu.Unlock()
				break
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rpc.ServeHTTP(w, r)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		s.releaseAll()
		s.srv.Close()
	})
	s.cardJSON = `{"name":"hello","description":"says hello","version":"1.0.0",
"supportedInterfaces":[{"url":"` + s.srv.URL + `/base/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":{"streaming":true},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],
"skills":[{"id":"hello","name":"hello","description":"hello","tags":["h"]}]}`
	return s
}

func (s *seller) gate(text string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gates[text]
	if !ok {
		g = make(chan struct{})
		s.gates[text] = g
	}
	return g
}

func (s *seller) release(text string) { close(s.gate(text)) }

func (s *seller) releaseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, g := range s.gates {
		select {
		case <-g:
		default:
			close(g)
		}
		delete(s.gates, k)
	}
}

func (s *seller) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (s *seller) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		text := ec.Message.Parts[0].Text()
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if text == "hi" {
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted,
				a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("Hello World"))), nil)
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		select {
		case <-s.gate(text):
		case <-ctx.Done():
			return
		}
		if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("done: "+text)), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

func (s *seller) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	s.mu.Lock()
	s.cancels++
	s.mu.Unlock()
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// testLog collects a replica's log and prints it when the test fails.
func testLog(t *testing.T, name string) *slog.Logger {
	var mu sync.Mutex
	var buf bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			mu.Lock()
			t.Logf("log of %s:\n%s", name, buf.String())
			mu.Unlock()
		}
	})
	return slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// gateEnv is one database, one Temporal namespace and a seller; replicas started on it share them.
type gateEnv struct {
	t        *testing.T
	dsn      string
	temporal string
	pool     *pgxpool.Pool
	seller   *seller
}

func newGateEnv(t *testing.T) *gateEnv {
	t.Helper()
	e := &gateEnv{t: t, temporal: temporaltest.Address(t), dsn: pgtest.Migrated(t)}
	e.pool = pgtest.Pool(t, e.dsn)
	e.seller = newSeller(t)
	return e
}

// replicaConfig writes a kontra.yaml for one replica and returns its path.
func (e *gateEnv) replicaConfig(port int, drain time.Duration, services string) string {
	e.t.Helper()
	if services == "" {
		services = e.helloService("org:default")
	}
	doc := fmt.Sprintf(`listen: 127.0.0.1:%d
public_url: http://127.0.0.1:%d
postgres: %s
temporal: { address: %s, namespace: kontra }
metrics: { listen: "127.0.0.1:0" }
auth: { api_keys: true }
drain_timeout: %s
%s`, port, port, e.dsn, e.temporal, drain, services)
	path := filepath.Join(e.t.TempDir(), "kontra.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *gateEnv) helloService(allow string) string {
	return `services:
  hello:
    kind: a2a
    backend: { a2a: { url: "` + e.seller.srv.URL + `/base", allow_private: true } }
    allow: ["` + allow + `"]
    task: { timeout: 10m, retries: 1 }
`
}

// running is a replica whose Run is in progress.
type running struct {
	r      *Replica
	url    string
	cancel context.CancelCauseFunc
	done   chan error
}

var fastTiming = Timing{Beat: 200 * time.Millisecond, Health: 200 * time.Millisecond, ServeGrace: time.Second,
	OrphanEvery: time.Hour, RetentionEvery: time.Hour}

func (e *gateEnv) start(name, path string, timing Timing) *running {
	e.t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := New(context.Background(), cfg, WithLogger(testLog(e.t, name)), WithTiming(timing), WithoutSIGHUP(),
		WithReload(func() (*config.Config, error) { return config.Load(path) }))
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	run := &running{r: r, url: "http://" + r.Addr().String(), cancel: cancel, done: make(chan error, 1)}
	go func() { run.done <- r.Run(ctx) }()
	e.t.Cleanup(func() {
		cancel(errors.New("test over"))
		select {
		case <-run.done:
		case <-time.After(90 * time.Second):
			e.t.Errorf("replica %s did not stop", name)
		}
	})
	eventually(e.t, 20*time.Second, "replica "+name+" ready", func() bool { return r.Ready() })
	return run
}

func (run *running) drain() { run.cancel(fmt.Errorf("%w: test", ErrDrain)) }

func (run *running) wait(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-run.done:
		run.done <- err
		return err
	case <-time.After(within):
		t.Fatalf("Run did not return within %v", within)
		return nil
	}
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mintKey mints an API key in the team tenant, as `kontra-gate enroll --api-key` does.
func (e *gateEnv) mintKey(name string) string {
	e.t.Helper()
	ctx := context.Background()
	tid, err := tenant.EnsureTeam(ctx, e.pool)
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := auth.NewAPIKeys(e.pool, nil).Mint(ctx, auth.MintRequest{TenantID: tid, Name: name, Actor: "test"})
	if err != nil {
		e.t.Fatal(err)
	}
	return m.Secret
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// client is a2a-go's client on the gate, the way `a2a send -a {svc}` reaches it: the card at {svc} (D10).
func a2aClient(t *testing.T, base, key string) *a2aclient.Client {
	t.Helper()
	hc := &http.Client{Transport: bearer{key}}
	card, err := agentcard.NewResolver(hc).Resolve(context.Background(), base+"/agents/hello")
	if err != nil {
		t.Fatalf("resolve the card at {svc}: %v", err)
	}
	c, err := a2aclient.NewFromCard(context.Background(), card, a2aclient.WithJSONRPCTransport(hc))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Destroy() })
	return c
}

func send(t *testing.T, c *a2aclient.Client, text string, returnImmediately bool) *a2a.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text)),
		Config: &a2a.SendMessageConfig{ReturnImmediately: returnImmediately}})
	if err != nil {
		t.Fatalf("SendMessage %q: %v", text, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage %q answered %T", text, res)
	}
	return task
}

func getTask(t *testing.T, c *a2aclient.Client, id a2a.TaskID) *a2a.Task {
	t.Helper()
	task, err := c.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

func healthz(t *testing.T, base string) int {
	t.Helper()
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// rpcStatus posts a JSON-RPC GetTask to the service and returns the HTTP status: 404 is the unknown-service
// answer (spec §2.1, D50).
func rpcStatus(t *testing.T, base, key string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/agents/hello", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"`+uuid.NewString()+`"}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func attemptReplica(t *testing.T, pool *pgxpool.Pool, id a2a.TaskID) string {
	t.Helper()
	var r *string
	err := pool.QueryRow(context.Background(), `select replica_id from kontra.attempts where task_id = $1 order by n desc limit 1`, string(id)).Scan(&r)
	if err != nil || r == nil {
		return ""
	}
	return *r
}

// A replica on Postgres and Temporal (spec §2.1, §13.5, §14.3; spec/tunnel.md §11): it refuses a database
// not at the embedded version; it registers in gate_replicas, beats, and reaps replicas silent for more than
// 30 s (closing their MCP sessions); /healthz and /metrics answer; a task reaches the seller through the
// polling-free streaming path and completes; SIGHUP applies a changed allow: list and a removed service
// without restart. Then SIGTERM drains it: draining_at, /healthz 503, kontra-direct takes no new dispatch,
// the dispatch in flight finishes while the replica still answers callers, and Run returns after it; the
// replica's row is deleted.
func TestReplicaServesReloadsAndDrains(t *testing.T) {
	if _, err := New(context.Background(), parseConfigDSN(t, pgtest.Database(t))); !errors.Is(err, migrate.ErrSchemaNotCurrent) {
		t.Fatalf("New on a database without the schema: %v", err)
	}

	e := newGateEnv(t)
	ctx := context.Background()
	stale, sess := "gone-0000000a", uuid.New()
	if _, err := e.pool.Exec(ctx, `insert into kontra.gate_replicas (replica_id, version, last_seen_at) values ($1, 'old', now() - interval '40 seconds'),
		('alive-0000000b', 'old', now())`, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, buyer_id, principal, via, protocol_version, replica_id)
		values (sha256($1), $2, 'anonymous', 'anonymous', '2025-11-25', $3)`, []byte(sess.String()), uuid.New(), stale); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	path := e.replicaConfig(port, time.Minute, "")
	run := e.start("a", path, fastTiming)
	r := run.r

	// gate_replicas: registered with the build version, beating; the silent replica reaped.
	var ver string
	if err := e.pool.QueryRow(ctx, `select version from kontra.gate_replicas where replica_id = $1 and draining_at is null`, r.ID()).Scan(&ver); err != nil || ver != version.Get() {
		t.Fatalf("gate_replicas row: %q %v", ver, err)
	}
	first := scalar[time.Time](t, e.pool, `select last_seen_at from kontra.gate_replicas where replica_id = $1`, r.ID())
	eventually(t, 5*time.Second, "last_seen_at moves", func() bool {
		return scalar[time.Time](t, e.pool, `select last_seen_at from kontra.gate_replicas where replica_id = $1`, r.ID()).After(first)
	})
	eventually(t, 5*time.Second, "the silent replica reaped", func() bool {
		return scalar[int](t, e.pool, `select count(*) from kontra.gate_replicas where replica_id = $1`, stale) == 0
	})
	if reason := scalar[string](t, e.pool, `select coalesce(close_reason, '') from kontra.mcp_sessions where replica_id = $1`, stale); reason != "tunnel_lost" {
		t.Fatalf("the reaped replica's MCP session: %q", reason)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.gate_replicas where replica_id = 'alive-0000000b'`); n != 1 {
		t.Fatal("a replica seen within 30 s was reaped")
	}

	if code := healthz(t, run.url); code != http.StatusOK {
		t.Fatalf("/healthz = %d", code)
	}
	resp, err := http.Get("http://" + r.MetricsAddr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(metrics, []byte(`kontra_gate_ready{replica="`+r.ID()+`"} 1`)) || !bytes.Contains(metrics, []byte("kontra_gate_services")) ||
		!bytes.Contains(metrics, []byte("# TYPE kontra_gate_operator_patches_refused_total counter")) {
		t.Fatalf("/metrics:\n%s", metrics)
	}
	if code := healthz(t, "http://"+r.MetricsAddr().String()); code != http.StatusNotFound {
		t.Fatalf("/healthz on the metrics listener = %d", code)
	}

	key := e.mintKey("buyer")
	c := a2aClient(t, run.url, key)
	done := send(t, c, "hi", false)
	if done.Status.State != a2a.TaskStateCompleted || done.Status.Message == nil || done.Status.Message.Parts[0].Text() != "Hello World" {
		t.Fatalf("hi: %s %+v", done.Status.State, done.Status.Message)
	}

	// SIGHUP: an allow: list that no longer admits the key, then back, then the service removed.
	if err := os.WriteFile(path, []byte(strings.Replace(mustRead(t, path), `allow: ["org:default"]`, `allow: ["key:kk_nobody_*"]`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if code := rpcStatus(t, run.url, key); code != http.StatusNotFound {
		t.Fatalf("a key the reloaded allow: list does not admit: HTTP %d, want the unknown-service 404", code)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(mustRead(t, path), `allow: ["key:kk_nobody_*"]`, `allow: ["org:default"]`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if code := rpcStatus(t, run.url, key); code != http.StatusOK {
		t.Fatalf("after restoring the allow: list: HTTP %d", code)
	}
	removed := e.replicaConfig(port, time.Minute, "services: {}\n")
	if err := os.WriteFile(path, []byte(mustRead(t, removed)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if code := rpcStatus(t, run.url, key); code != http.StatusNotFound {
		t.Fatalf("a removed service: HTTP %d, want 404", code)
	}
	if !scalar[bool](t, e.pool, `select disabled_at is not null from kontra.services where name = 'hello'`) {
		t.Fatal("the removed service has no disabled_at")
	}
	if err := os.WriteFile(path, []byte(mustRead(t, e.replicaConfig(port, time.Minute, ""))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	// The drain. A task in flight, then SIGTERM.
	slow := send(t, c, "slow", true)
	eventually(t, 20*time.Second, "the slow task's dispatch runs here", func() bool {
		return r.InFlight() == 1 && attemptReplica(t, e.pool, slow.ID) == r.ID()
	})
	run.drain()
	eventually(t, 5*time.Second, "/healthz 503 while draining", func() bool { return healthz(t, run.url) == http.StatusServiceUnavailable })
	eventually(t, 5*time.Second, "gate_replicas.draining_at set", func() bool {
		return scalar[bool](t, e.pool, `select draining_at is not null from kontra.gate_replicas where replica_id = $1`, r.ID())
	})
	// kontra-direct no longer polls: a new task is accepted (kontra-lifecycle still polls) but its dispatch
	// does not start here.
	later := send(t, c, "later", true)
	time.Sleep(3 * time.Second)
	if got := attemptReplica(t, e.pool, later.ID); got != "" || r.InFlight() != 1 {
		t.Fatalf("a draining replica took a new dispatch (replica %q, in flight %d)", got, r.InFlight())
	}
	select {
	case err := <-run.done:
		t.Fatalf("Run returned with a dispatch in flight: %v", err)
	default:
	}
	// The replica still answers its callers while it drains.
	if st := getTask(t, c, slow.ID).Status.State; st != a2a.TaskStateWorking {
		t.Fatalf("GetTask during the drain: %s", st)
	}
	// The dispatch in flight finishes and its result is recorded; then the replica exits.
	e.seller.release("slow")
	if err := run.wait(t, 30*time.Second); err != nil {
		t.Fatalf("Run after the drain: %v", err)
	}
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, string(slow.ID)); st != "completed" {
		t.Fatalf("the dispatch that was in flight at SIGTERM ended %s", st)
	}
	if o := scalar[string](t, e.pool, `select coalesce(outcome, '') from kontra.attempts where task_id = $1`, string(slow.ID)); o != "completed" {
		t.Fatalf("its attempt ended %q", o)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.gate_replicas where replica_id = $1`, r.ID()); n != 0 {
		t.Fatal("the drained replica's row is still in gate_replicas")
	}
	if code := healthz(t, run.url); code != 0 {
		t.Fatalf("the public listener still answers after Run returned: %d", code)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func parseConfigDSN(t *testing.T, dsn string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("public_url: http://localhost:8080\npostgres: "+dsn+"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// On a gate of one replica, a cancel still works during a drain (spec §14.3, D76; the audit's single-replica
// drain test): kontra-lifecycle accepts the kontra.cancel Update, forward_cancel reaches this replica's
// kontra-ctl queue, the seller receives CancelTask, the task ends canceled with no completed after, and
// the replica exits once that dispatch has ended.
func TestDrainServesCancelOnOneReplica(t *testing.T) {
	e := newGateEnv(t)
	run := e.start("solo", e.replicaConfig(freePort(t), time.Minute, ""), fastTiming)
	key := e.mintKey("buyer")
	c := a2aClient(t, run.url, key)
	slow := send(t, c, "slow-cancel", true)
	eventually(t, 20*time.Second, "the dispatch runs", func() bool {
		return run.r.InFlight() == 1 && attemptReplica(t, e.pool, slow.ID) == run.r.ID()
	})
	eventually(t, 10*time.Second, "the seller task is working", func() bool {
		return getTask(t, c, slow.ID).Status.State == a2a.TaskStateWorking
	})
	run.drain()
	eventually(t, 5*time.Second, "draining", func() bool { return run.r.Draining() })

	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	got, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: slow.ID})
	if err != nil {
		t.Fatalf("CancelTask during the drain: %v", err)
	}
	if got.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("CancelTask answered %s", got.Status.State)
	}
	if e.seller.count("CancelTask") != 1 {
		t.Fatalf("the seller received %d CancelTask", e.seller.count("CancelTask"))
	}
	if err := run.wait(t, 30*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	e.seller.release("slow-cancel") // a seller that ignored the cancel would now complete: nothing may follow
	time.Sleep(500 * time.Millisecond)
	var states []string
	rows, err := e.pool.Query(context.Background(), `select payload::text from kontra.task_events where task_id = $1 and kind = 'status' order by seq`, string(slow.ID))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		states = append(states, p)
	}
	last := states[len(states)-1]
	if !strings.Contains(last, "TASK_STATE_CANCELED") {
		t.Fatalf("the last status is not canceled: %s", last)
	}
	for _, s := range states {
		if strings.Contains(s, "TASK_STATE_COMPLETED") {
			t.Fatalf("a completed status was logged after the cancel: %v", states)
		}
	}
}

// When drain_timeout ends with a dispatch still running, the dispatch is a lost session like any other
// (spec §14.3): its context ends, and another replica re-attaches to the upstream task (direct backend,
// D42), which then completes there.
func TestDrainTimeoutReattachesElsewhere(t *testing.T) {
	e := newGateEnv(t)
	a := e.start("a", e.replicaConfig(freePort(t), 2*time.Second, ""), fastTiming)
	key := e.mintKey("buyer")
	ca := a2aClient(t, a.url, key)
	slow := send(t, ca, "slow-reattach", true)
	eventually(t, 20*time.Second, "the dispatch runs on a", func() bool {
		return a.r.InFlight() == 1 && attemptReplica(t, e.pool, slow.ID) == a.r.ID()
	})
	b := e.start("b", e.replicaConfig(freePort(t), time.Minute, ""), fastTiming)
	a.drain()
	if err := a.wait(t, 30*time.Second); err != nil {
		t.Fatalf("Run of the drained replica: %v", err)
	}
	cb := a2aClient(t, b.url, key)
	eventually(t, 60*time.Second, "b re-attaches the dispatch", func() bool { return attemptReplica(t, e.pool, slow.ID) == b.r.ID() })
	e.seller.release("slow-reattach")
	eventually(t, 30*time.Second, "the task completes on b", func() bool {
		return getTask(t, cb, slow.ID).Status.State == a2a.TaskStateCompleted
	})
	if n := e.seller.count("SendStreamingMessage") + e.seller.count("SendMessage"); n != 1 {
		t.Fatalf("the seller received the message %d times; a re-attach never sends it again", n)
	}
}

// proxy is a TCP forwarder the test can cut and restore, to make Postgres or Temporal unreachable.
type proxy struct {
	t      *testing.T
	target string
	addr   string
	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]bool
}

func newProxy(t *testing.T, target string) *proxy {
	p := &proxy{t: t, target: target, conns: map[net.Conn]bool{}}
	p.listen("127.0.0.1:0")
	t.Cleanup(p.cut)
	return p
}

func (p *proxy) listen(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	p.ln, p.addr = ln, ln.Addr().String()
	p.mu.Unlock()
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", p.target)
			if err != nil {
				in.Close()
				continue
			}
			p.mu.Lock()
			p.conns[in], p.conns[out] = true, true
			p.mu.Unlock()
			go func() { _, _ = io.Copy(out, in); out.Close(); in.Close() }()
			go func() { _, _ = io.Copy(in, out); out.Close(); in.Close() }()
		}
	}()
}

func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		c.Close()
		delete(p.conns, c)
	}
}

func (p *proxy) restore() { p.listen(p.addr) }

// /healthz is 503 while Postgres or Temporal is unreachable, and 200 again once it is back (spec §2.1).
func TestHealthzFollowsPostgresAndTemporal(t *testing.T) {
	e := newGateEnv(t)
	u, err := url.Parse(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	pgProxy := newProxy(t, u.Host)
	tProxy := newProxy(t, e.temporal)
	u.Host = pgProxy.addr
	e.dsn, e.temporal = u.String(), tProxy.addr
	run := e.start("h", e.replicaConfig(freePort(t), time.Minute, ""), fastTiming)
	if code := healthz(t, run.url); code != http.StatusOK {
		t.Fatalf("/healthz = %d", code)
	}
	tProxy.cut()
	eventually(t, 15*time.Second, "503 while Temporal is unreachable", func() bool { return healthz(t, run.url) == http.StatusServiceUnavailable })
	tProxy.restore()
	eventually(t, 60*time.Second, "200 once Temporal is back", func() bool { return healthz(t, run.url) == http.StatusOK })
	pgProxy.cut()
	eventually(t, 15*time.Second, "503 while Postgres is unreachable", func() bool { return healthz(t, run.url) == http.StatusServiceUnavailable })
	pgProxy.restore()
	eventually(t, 60*time.Second, "200 once Postgres is back", func() bool { return healthz(t, run.url) == http.StatusOK })
}

// The edge's limits.body is the only body limit of a wired replica (WP-06's note; spec §2.2): with
// limits.body at 8 MiB, a 5 MiB body reaches the A2A routes on JSON-RPC, message:send and message:stream
// (which then answer the missing A2A-Version, -32009), and a 9 MiB body is the edge's 413 on each.
func TestBodyLimitIsTheConfiguredOne(t *testing.T) {
	e := newGateEnv(t)
	path := e.replicaConfig(freePort(t), time.Minute, "limits: { body: 8MiB }\n"+e.helloService("org:default"))
	run := e.start("body", path, fastTiming)
	key := e.mintKey("buyer")
	body := func(size int, rpc bool) string {
		pad := strings.Repeat("x", size)
		msg := `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}],"metadata":{"pad":"` + pad + `"}}}`
		if rpc {
			return `{"jsonrpc":"2.0","id":7,"method":"SendMessage","params":` + msg + `}`
		}
		return msg
	}
	for _, route := range []struct {
		path string
		rpc  bool
	}{{"/agents/hello", true}, {"/agents/hello/message:send", false}, {"/agents/hello/message:stream", false}} {
		for _, tc := range []struct {
			size     int
			status   int
			contains string
		}{
			{5 << 20, map[bool]int{true: http.StatusOK, false: http.StatusBadRequest}[route.rpc], "A2A-Version"},
			{9 << 20, http.StatusRequestEntityTooLarge, ""},
		} {
			req, _ := http.NewRequest(http.MethodPost, run.url+route.path, strings.NewReader(body(tc.size, route.rpc)))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", route.path, tc.size, err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status || !strings.Contains(string(got), tc.contains) {
				t.Fatalf("%s, %d MiB: HTTP %d %.200s; want %d with %q", route.path, tc.size>>20, resp.StatusCode, got, tc.status, tc.contains)
			}
		}
	}
}
