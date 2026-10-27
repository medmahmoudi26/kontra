package mcprelay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/peer"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/upstream"
)

// The relay's integration harness: one gate database (pgtest), its team tenant, API keys, MCP services (direct
// backends on a mcptest double, tunnel ones), and replicas, each a Handler behind the edge middleware on a test
// server with its own tunnel registry, revocation view and LISTEN connection, and, when asked, a peer listener.
// Nodes are FakeNodes registered on a replica's registry over a real yamux pair.

var peerSecret = []byte("peer-secret-for-tests-0123456789abcdef")

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	team  uuid.UUID
	keys  *auth.APIKeys
	store *enroll.Store
	svcs  *memServices
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	team, err := tenant.EnsureTeam(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, pool: pool, team: team, keys: auth.NewAPIKeys(pool, nil),
		store: enroll.NewStore(pool, enroll.Options{PublicURL: "http://gate.test"}), svcs: &memServices{m: map[string]*Service{}}}
}

// memServices is the registry the tests configure (internal/gate's builds it from the rows).
type memServices struct {
	mu sync.Mutex
	m  map[string]*Service
}

func (s *memServices) MCPService(_ context.Context, tn, name string) (*Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tn != tenant.TeamSlug {
		return nil, nil
	}
	return s.m[name], nil
}

func (s *memServices) MCPServiceByID(_ context.Context, _ uuid.UUID, name string) (*Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name], nil
}

// apiKey mints an API key; it returns the secret and the principal.
func (e *env) apiKey(name string) (string, string) {
	e.t.Helper()
	m, err := e.keys.Mint(context.Background(), auth.MintRequest{TenantID: e.team, Name: name, Actor: "test"})
	if err != nil {
		e.t.Fatal(err)
	}
	return m.Secret, m.Principal()
}

// service adds an MCP service: direct on url when url is not empty, else a tunnel service. Its row is written
// (mcp_sessions and calls reference it), and mod adjusts what the relay serves.
func (e *env) service(name, url string, mod func(*Service)) *Service {
	e.t.Helper()
	backend := `{"tunnel": {}}`
	if url != "" {
		b, _ := json.Marshal(map[string]any{"mcp": map[string]any{"url": url, "allow_private": true}})
		backend = string(b)
	}
	if _, err := e.pool.Exec(context.Background(), `insert into kontra.services (tenant_id, name, kind, backend, allow, tasks_mode, source)
		values ($1, $2, 'mcp', $3::jsonb, '{org:team}', 'durable', 'config')`, e.team, name, backend); err != nil {
		e.t.Fatal(err)
	}
	allow, err := auth.ParseAllowList([]string{"org:" + tenant.TeamSlug})
	if err != nil {
		e.t.Fatal(err)
	}
	svc := &Service{Ref: store.ServiceRef{TenantID: e.team, Tenant: tenant.TeamSlug, Name: name, Kind: config.KindMCP},
		Allow: allow, Tasks: config.TasksDurable, SessionIdle: time.Hour}
	if url != "" {
		be, err := upstream.NewDirect(url, true)
		if err != nil {
			e.t.Fatal(err)
		}
		svc.Direct = be
	}
	if mod != nil {
		mod(svc)
	}
	e.svcs.mu.Lock()
	e.svcs.m[name] = svc
	e.svcs.mu.Unlock()
	return svc
}

// logBuf is a replica's log, read by the tests that check what never appears in it.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type replica struct {
	id       string
	reg      *tunnelgate.Registry
	rev      *enroll.Revocations
	h        *Handler
	url      string
	peerURL  string // the peer listener's base URL, "" without one
	logs     *logBuf
	admitted *fakeAdmission
	// dispatcher is the replica's MCP dispatcher when it runs gate-owned tasks (tasks_pg_test.go).
	dispatcher *Dispatcher
	tools      *ToolsCache
	eraDrops   atomic.Int64 // the registry's MCP change hooks that ran (a node connected or disconnected)
}

type replicaOpts struct {
	// unsubscribed leaves the registry deaf to revocations (its own statements never run): only the stream checks
	// read the revocation view.
	unsubscribed bool
	peer         bool
	origins      []string
	admission    *fakeAdmission
	ops          []hooks.Operation
	drainTimeout time.Duration // the tools cache's bound on one drain (DrainTimeout when 0)
	probeTimeout time.Duration // the bound on the gate's own server/discover (ProbeTimeout when 0)
	// withDeps, when set, completes the handler's deps before it is built (tasks_pg_test.go: gate-owned tasks and
	// the replica's Temporal workers), with the replica's LISTEN connection and logger.
	withDeps func(r *replica, deps *Deps, l *pg.Listener, logger *slog.Logger)
	// now, when set, is the handler's clock.
	now func() time.Time
}

// replica starts one replica over the env's database.
func (e *env) replica(name string, o replicaOpts) *replica {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{id: "r-" + name + "-" + uuid.NewString()[:8], logs: &logBuf{}, admitted: o.admission}
	if _, err := e.pool.Exec(ctx, `insert into kontra.gate_replicas (replica_id, version) values ($1, 'test')`, r.id); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})).With("replica", name)
	r.rev = enroll.NewRevocations(e.pool, enroll.WithRevocationsLogger(logger))
	r.reg = tunnelgate.NewRegistry(tunnelgate.RegistryConfig{DB: e.pool, ReplicaID: r.id, Revocations: r.rev, Logger: logger})
	if !o.unsubscribed {
		r.rev.Subscribe(r.reg.Revoked)
	}
	l := pg.NewListener(e.pool.Config().ConnConfig, pg.WithLogger(logger))
	l.OnChannel(pg.ChannelRevoke, r.rev.Notify)
	l.OnChannel(pg.ChannelTunnel, r.reg.Notify)
	l.OnReconnect(r.rev.Catchup)
	var wg sync.WaitGroup
	wg.Go(func() { _ = l.Run(ctx) })
	wg.Go(func() { _ = r.rev.Run(ctx) })
	wg.Go(func() { _ = r.reg.Run(ctx) })

	tools := NewToolsCache()
	if o.drainTimeout > 0 {
		tools.drainTimeout = o.drainTimeout
	}
	if o.probeTimeout > 0 {
		tools.probeTimeout = o.probeTimeout
	}
	// As internal/gate wires it: a node of an MCP service connecting or disconnecting drops its era (D177).
	r.reg.OnMCPChange(func(tenantID uuid.UUID, service string) {
		tools.DropEra(ToolsKey{TenantID: tenantID, Service: service})
		r.eraDrops.Add(1)
	})
	r.tools = tools
	deps := Deps{
		ReplicaID: r.id, Services: e.svcs, Auth: auth.NewAuthenticator(e.keys), Sessions: NewSessions(e.pool),
		Tools: tools, Meter: NewMeter(e.pool), Tunnels: r.reg, Picker: tunnelgate.NewPicker(e.pool),
		AllowedOrigins: o.origins, Log: logger, AdmissionOps: o.ops,
	}
	if o.admission != nil {
		deps.Admission = o.admission
	}
	if o.now != nil {
		deps.now = o.now
	}
	var signer *peer.Signer
	if o.peer {
		signer, _ = peer.NewSigner(peerSecret)
		deps.Peer, _ = peer.NewClient(signer)
	}
	if o.withDeps != nil {
		o.withDeps(r, &deps, l, logger)
	}
	h, err := NewHandler(deps)
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	routes := edge.NewRouter()
	h.Register(routes)
	pub := httptest.NewServer(edge.Middleware(edge.MiddlewareConfig{}, routes))
	r.url = pub.URL
	if o.peer {
		ps, err := peer.NewServer(signer, h, peer.WithLogger(logger))
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(ps)
		r.peerURL = srv.URL
		if _, err := e.pool.Exec(ctx, `update kontra.gate_replicas set peer_addr = $2 where replica_id = $1`,
			r.id, strings.TrimPrefix(srv.URL, "http://")); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
	}
	// Keep the replica live (spec/tunnel.md §11: seen within 30 s) for the test's length.
	wg.Go(func() {
		tk := time.NewTicker(5 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				_, _ = e.pool.Exec(context.Background(), `update kontra.gate_replicas set last_seen_at = now() where replica_id = $1`, r.id)
			}
		}
	})
	t.Cleanup(func() {
		pub.Close()
		cancel()
		wg.Wait()
		if t.Failed() {
			t.Logf("replica %s log:\n%s", name, r.logs.String())
		}
	})
	eventually(t, 10*time.Second, "the replica's LISTEN connection and revocation view are up", func() bool {
		return l.Healthy() && r.rev.Fresh()
	})
	return r
}

// node is a FakeNode connected to a replica: a tunnel_keys row enrolled for services, a yamux pair, and the
// session registered in the replica's registry (a candidate once Add returns).
type node struct {
	key     string
	session *tunnelgate.Session
	fake    *mcptest.FakeNode
	gateMux interface{ Close() error }
}

func (e *env) node(r *replica, h http.Handler, services ...string) *node {
	t := e.t
	t.Helper()
	ctx := context.Background()
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	key := "k1_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))[:26]
	pub := make([]byte, 32)
	_, _ = rand.Read(pub)
	if _, err := e.pool.Exec(ctx, `insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ($1, $2, $3, 'test')`,
		key, e.team, pub); err != nil {
		t.Fatal(err)
	}
	var served []tunnel.WelcomeService
	for _, s := range services {
		if _, err := e.pool.Exec(ctx, `insert into kontra.key_services (key_id, tenant_id, service) values ($1, $2, $3)`, key, e.team, s); err != nil {
			t.Fatal(err)
		}
		served = append(served, tunnel.WelcomeService{Kind: tunnel.KindMCP, Name: s})
	}
	pair := tunneltest.NewPair(t)
	nodeMux, gateMux := pair.Sessions(t, nil)
	fake := &mcptest.FakeNode{Handler: h}
	go fake.Serve(nodeMux)
	id, _ := uuid.NewV7()
	s := &tunnelgate.Session{ID: id, KeyID: key, TenantID: e.team, Tenant: tenant.TeamSlug, CLIVersion: "test"}
	if err := r.reg.Add(ctx, s, gateMux, served); err != nil {
		t.Fatal(err)
	}
	return &node{key: key, session: s, fake: fake, gateMux: gateMux}
}

// fakeAdmission is a hooks.Admission whose decision the test sets (C3: Admission is tested with a fake).
type fakeAdmission struct {
	mu       sync.Mutex
	decision hooks.Decision
	err      error
	asked    []hooks.AdmitRequest
}

func (f *fakeAdmission) Admit(_ context.Context, req hooks.AdmitRequest) (hooks.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, req)
	if f.decision.Outcome == "" && f.err == nil {
		return hooks.Decision{Outcome: hooks.Allow}, nil
	}
	return f.decision, f.err
}

func (f *fakeAdmission) set(d hooks.Decision, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decision, f.err = d, err
}

func (f *fakeAdmission) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// client speaks MCP 2025-11-25 to a replica as a stock client does.
type client struct {
	t   *testing.T
	key string
	sid string
	hc  *http.Client
}

func newClient(t *testing.T, key string) *client {
	return &client{t: t, key: key, hc: &http.Client{Timeout: 30 * time.Second}}
}

// reply is a response as the client read it.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// msg decodes the JSON-RPC message of a JSON reply, or of the last event of an event stream.
func (r reply) msg(t *testing.T) map[string]any {
	t.Helper()
	b := r.body
	if strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") {
		var last string
		for _, l := range strings.Split(string(b), "\n") {
			if d, ok := strings.CutPrefix(l, "data: "); ok && strings.TrimSpace(d) != "" {
				last = d
			}
		}
		b = []byte(last)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON-RPC message (%d): %q", r.status, r.body)
	}
	return m
}

func (r reply) errCode(t *testing.T) int {
	t.Helper()
	e, _ := r.msg(t)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error in %s", r.body)
	}
	return int(e["code"].(float64))
}

func (c *client) do(method, url, body string, hdr map[string]string) reply {
	c.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	if c.sid != "" {
		req.Header.Set("Mcp-Session-Id", c.sid)
		req.Header.Set("MCP-Protocol-Version", mcptest.Version)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: b}
}

// initialize opens a session at url and keeps its id.
func (c *client) initialize(url string) reply {
	c.t.Helper()
	c.sid = ""
	r := c.do(http.MethodPost, url, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
		`"capabilities":{"elicitation":{}},"clientInfo":{"name":"test","version":"1"}}}`, map[string]string{"MCP-Protocol-Version": ""})
	if r.status != http.StatusOK {
		c.t.Fatalf("initialize: %d %s", r.status, r.body)
	}
	c.sid = r.header.Get("Mcp-Session-Id")
	if c.sid == "" {
		c.t.Fatalf("initialize: no Mcp-Session-Id: %s", r.body)
	}
	return r
}

func (c *client) call(url string, id int, tool, text string) reply {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{"text": text}}})
	return c.do(http.MethodPost, url, string(b), nil)
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// row is an mcp_sessions row as the tests read it.
type row struct {
	upstreamSID string
	replica     *string
	tunnel      *uuid.UUID
	closed      bool
	reason      *string
	version     string
}

func (e *env) row(sid string) row {
	e.t.Helper()
	h := HashID(sid)
	var r row
	if err := e.pool.QueryRow(context.Background(), `select coalesce(upstream_sid, ''), replica_id, tunnel_session, closed_at is not null,
		close_reason, protocol_version from kontra.mcp_sessions where client_sid_hash = $1`, h[:]).Scan(
		&r.upstreamSID, &r.replica, &r.tunnel, &r.closed, &r.reason, &r.version); err != nil {
		e.t.Fatalf("mcp_sessions row of %s: %v", sid, err)
	}
	return r
}

// calls are the calls rows of a service, oldest first.
type callRow struct {
	tool     string
	isError  *bool
	bytesIn  int64
	bytesOut int64
	session  []byte
	task     *uuid.UUID
}

func (e *env) calls(service string) []callRow {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `select tool, is_error, bytes_in, bytes_out, mcp_session, task_id
		from kontra.calls where service = $1 order by at, id`, service)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []callRow
	for rows.Next() {
		var c callRow
		if err := rows.Scan(&c.tool, &c.isError, &c.bytesIn, &c.bytesOut, &c.session, &c.task); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}
