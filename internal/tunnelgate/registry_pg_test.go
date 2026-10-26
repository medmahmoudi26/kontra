package tunnelgate

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnel/tunneltest"
)

// tunnelEnv is one gate's database (the team tenant, its tunnel services, the enrollment store) and the
// replicas over it: each a Registry behind a Handler, with its own revocation view and LISTEN connection.
type tunnelEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *enroll.Store
	team  uuid.UUID
}

// testCard is an upstream card a test node's agent serves.
const testCard = `{"name":"hello","description":"says hello","version":"1.0.0",
"supportedInterfaces":[{"url":"http://127.0.0.1:9999/","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":{"streaming":true},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`

func newTunnelEnv(t *testing.T, services ...string) *tunnelEnv {
	t.Helper()
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	team, err := tenant.EnsureTeam(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range services {
		if _, err := pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source)
			values ($1, $2, 'a2a', '{"tunnel": {}}'::jsonb, 'config')`, team, s); err != nil {
			t.Fatal(err)
		}
	}
	return &tunnelEnv{t: t, pool: pool, store: enroll.NewStore(pool, enroll.Options{PublicURL: "http://gate.test"}), team: team}
}

// replica is one gate replica's tunnel side.
type replica struct {
	id   string
	reg  *Registry
	rev  *enroll.Revocations
	gate *testGate
	logs *logBuf

	draining     atomic.Bool
	tunnelNotify atomic.Bool // NOTIFY kontra_tunnel reaches the registry
	revokeNotify atomic.Bool // NOTIFY kontra_revoke reaches the revocation view
}

type replicaOpts struct {
	every     time.Duration // LastPingEvery
	pingEvery time.Duration // the Handler's ping loop
	cardWait  time.Duration
}

func (e *tunnelEnv) replica(name string, o replicaOpts) *replica {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{id: "r-" + name + "-" + uuid.NewString()[:8], logs: &logBuf{}}
	r.tunnelNotify.Store(true)
	r.revokeNotify.Store(true)
	if _, err := e.pool.Exec(ctx, `insert into kontra.gate_replicas (replica_id, version) values ($1, 'test')`, r.id); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})).With("replica", name)
	r.rev = enroll.NewRevocations(e.pool, enroll.WithRevocationsLogger(logger))
	r.reg = NewRegistry(RegistryConfig{DB: e.pool, ReplicaID: r.id, Revocations: r.rev, Logger: logger,
		LastPingEvery: o.every, CardWait: o.cardWait})
	r.rev.Subscribe(r.reg.Revoked)
	l := pg.NewListener(e.pool.Config().ConnConfig, pg.WithLogger(logger))
	l.OnChannel(pg.ChannelRevoke, func(p string) {
		if r.revokeNotify.Load() {
			r.rev.Notify(p)
		}
	})
	l.OnChannel(pg.ChannelTunnel, func(p string) {
		if r.tunnelNotify.Load() {
			r.reg.Notify(p)
		}
	})
	l.OnReconnect(r.rev.Catchup)
	var wg sync.WaitGroup
	wg.Go(func() { _ = l.Run(ctx) })
	wg.Go(func() { _ = r.rev.Run(ctx) })
	wg.Go(func() { _ = r.reg.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		if t.Failed() {
			t.Logf("replica %s log:\n%s", name, r.logs.String())
		}
	})
	r.gate = newTestGate(t, gateOpts{admitter: e.store, mod: func(c *HandlerConfig) {
		c.Registry = r.reg
		c.Draining = r.draining.Load
		if o.pingEvery > 0 {
			c.PingEvery = o.pingEvery
		}
	}})
	eventually(t, 10*time.Second, "the replica's LISTEN connection and revocation view are up", func() bool {
		return l.Healthy() && r.rev.Fresh()
	})
	return r
}

// testNode is a node over a completed handshake: it reads the control stream into controls, records the
// preamble of every other stream the gate opens, and serves each as one HTTP/1.1 exchange with agent.
type testNode struct {
	id        *nodekey.Identity
	mux       *yamux.Session
	welcome   *tunnel.Welcome
	session   uuid.UUID
	controls  chan tunnel.Control
	preambles chan tunnel.Preamble
}

func (r *replica) startNode(t *testing.T, id *nodekey.Identity, hello *tunnel.Hello, agent http.Handler) (*testNode, error) {
	t.Helper()
	d, w, err := r.gate.connect(t, id, hello, false)
	if err != nil {
		return nil, err
	}
	m, err := tunnel.ClientSession(context.Background(), d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := &testNode{id: id, mux: m, welcome: w, session: uuid.MustParse(w.Session),
		controls: make(chan tunnel.Control, 64), preambles: make(chan tunnel.Preamble, 64)}
	t.Cleanup(func() { _ = m.Close() })
	go n.accept(agent)
	return n, nil
}

func (n *testNode) accept(agent http.Handler) {
	first := true
	for {
		st, err := n.mux.AcceptStream()
		if err != nil {
			return
		}
		br := bufio.NewReader(st)
		if first {
			first = false
			go func() {
				p, err := tunnel.ReadPreamble(br)
				if err != nil || p.Op != tunnel.OpControl {
					_ = st.Close()
					return
				}
				for {
					c, err := tunnel.ReadControl(br)
					if err != nil {
						return
					}
					n.controls <- c
				}
			}()
			continue
		}
		go func() {
			p, err := tunnel.ReadPreamble(br)
			if err != nil {
				_ = st.Close()
				return
			}
			select {
			case n.preambles <- p:
			default:
			}
			serveOne(&prefixed{Conn: st, r: br}, agent)
		}()
	}
}

// prefixed reads through the reader that read the preamble.
type prefixed struct {
	net.Conn
	r *bufio.Reader
}

func (p *prefixed) Read(b []byte) (int, error) { return p.r.Read(b) }

// serveOne serves one HTTP/1.1 exchange on c with h.
func serveOne(c net.Conn, h http.Handler) {
	l := &oneConn{ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- &closeNotify{Conn: c, l: l}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	_ = srv.Serve(l)
}

type oneConn struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConn) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *oneConn) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *oneConn) Addr() net.Addr { return &net.TCPAddr{} }

type closeNotify struct {
	net.Conn
	l *oneConn
}

func (c *closeNotify) Close() error { _ = c.l.Close(); return c.Conn.Close() }

// nextControl waits for the node's next control line.
func (n *testNode) nextControl(t *testing.T, within time.Duration) tunnel.Control {
	t.Helper()
	select {
	case c := <-n.controls:
		return c
	case <-time.After(within):
		t.Fatalf("no control line within %v", within)
		return tunnel.Control{}
	}
}

// noControl checks that the node reads no control line for d.
func (n *testNode) noControl(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case c := <-n.controls:
		t.Fatalf("unexpected control line %+v", c)
	case <-time.After(d):
	}
}

func (n *testNode) closed(within time.Duration) bool {
	select {
	case <-n.mux.CloseChan():
		return true
	case <-time.After(within):
		return false
	}
}

// agent is a node's local service: its card, and /slow, an event stream that sends one event and then
// waits for release (or the end of the request).
type agent struct {
	mu       sync.Mutex
	card     string
	cardGets int
	release  chan struct{}
	started  chan struct{}
}

func newAgent() *agent {
	return &agent{card: testCard, release: make(chan struct{}), started: make(chan struct{}, 16)}
}

func (a *agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/agent-card.json":
		a.mu.Lock()
		a.cardGets++
		c := a.card
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, c)
	case "/slow":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		a.started <- struct{}{}
		select {
		case <-a.release:
			_, _ = io.WriteString(w, "data: last\n\n")
		case <-r.Context().Done():
		}
	default:
		http.NotFound(w, r)
	}
}

func (a *agent) gets() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cardGets
}

// enrollNode mints a service token per service and connects id with them on r.
func (e *tunnelEnv) enrollNode(t *testing.T, r *replica, id *nodekey.Identity, a http.Handler, services ...string) *testNode {
	t.Helper()
	h := newHello(services...)
	for _, s := range services {
		tok, err := e.store.MintServiceToken(context.Background(), e.team, s, "test")
		if err != nil {
			t.Fatal(err)
		}
		h.Enroll = append(h.Enroll, tok)
	}
	n, err := r.startNode(t, id, h, a)
	if err != nil {
		t.Fatalf("enrolling handshake: %v", err)
	}
	e.waitRow(t, n.session)
	return n
}

// connectNode connects an enrolled key on r, without tokens.
func (e *tunnelEnv) connectNode(t *testing.T, r *replica, id *nodekey.Identity, a http.Handler, services ...string) *testNode {
	t.Helper()
	n, err := r.startNode(t, id, newHello(services...), a)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	e.waitRow(t, n.session)
	return n
}

func (e *tunnelEnv) hasRow(t *testing.T, session uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := e.pool.QueryRow(context.Background(), `select exists (select 1 from kontra.tunnels where session_id = $1)`, session).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func (e *tunnelEnv) waitRow(t *testing.T, session uuid.UUID) {
	t.Helper()
	eventually(t, 15*time.Second, "the session's tunnels row", func() bool { return e.hasRow(t, session) })
}

func (e *tunnelEnv) rowServices(t *testing.T, session uuid.UUID) []string {
	t.Helper()
	var s []string
	if err := e.pool.QueryRow(context.Background(), `select services from kontra.tunnels where session_id = $1`, session).Scan(&s); err != nil {
		t.Fatal(err)
	}
	slices.Sort(s)
	return s
}

// held is the registry's session of a node.
func (r *replica) held(t *testing.T, n *testNode) *Session {
	t.Helper()
	s, ok := r.reg.Get(n.session)
	if !ok {
		t.Fatalf("replica %s does not hold session %s", r.id, n.session)
	}
	return s
}

// exchangeClient sends HTTP requests on fresh streams of s for service.
func exchangeClient(s *Session, service string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: service, Kind: tunnel.KindA2A})
		},
		DisableKeepAlives: true,
	}}
}

// slowExchange starts GET /slow on s for service and returns the body after its first event.
func slowExchange(t *testing.T, s *Session, service string, a *agent) *bufio.Reader {
	t.Helper()
	resp, err := exchangeClient(s, service).Get(exchangeOrigin + "/slow")
	if err != nil {
		t.Fatalf("GET /slow for %s: %v", service, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first event %q, %v", line, err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not start the slow exchange")
	}
	return br
}

// readsGone checks that the rest of a slow exchange's body fails with ErrSessionGone within d.
func readsGone(t *testing.T, br *bufio.Reader, d time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(br)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionGone) {
			t.Fatalf("the in-flight exchange ended with %v, want ErrSessionGone", err)
		}
	case <-time.After(d):
		t.Fatalf("the in-flight exchange was not aborted within %v", d)
	}
}

// Add inserts the tunnels row (§11) with services = the welcome's, hire_principal and cli_version; the
// session is then a candidate (Get). last_ping_at moves once per LastPingEvery, not per ping, although the
// ping loop runs ten times as often. Session.Close deletes the row and closes the node's session.
func TestRegistryRowsAndLastPing(t *testing.T) {
	e := newTunnelEnv(t, "hello", "recon")
	const every = 400 * time.Millisecond
	a := e.replica("a", replicaOpts{every: every, pingEvery: 40 * time.Millisecond})
	id := newIdentity(t)
	n := e.enrollNode(t, a, id, newAgent(), "hello", "recon")

	var (
		key, replica, hire, cli string
		services                []string
		tid                     uuid.UUID
	)
	if err := e.pool.QueryRow(context.Background(), `select key_id, tenant_id, replica_id, services, hire_principal, cli_version
		from kontra.tunnels where session_id = $1`, n.session).Scan(&key, &tid, &replica, &services, &hire, &cli); err != nil {
		t.Fatal(err)
	}
	slices.Sort(services)
	if key != id.KeyID() || tid != e.team || replica != a.id || !slices.Equal(services, []string{"hello", "recon"}) ||
		hire != "node:"+id.KeyID() || cli != newHello().Agent {
		t.Fatalf("row: key %s tenant %s replica %s services %v hire %q cli %q", key, tid, replica, services, hire, cli)
	}
	s := a.held(t, n)
	if got := s.Served(); !slices.Equal(got, []string{"hello", "recon"}) {
		t.Fatalf("served %v", got)
	}

	// last_ping_at: sampled for 2.4 s, while the session pings every 40 ms.
	var stamps []time.Time
	deadline := time.Now().Add(6 * every)
	for time.Now().Before(deadline) {
		var at time.Time
		if err := e.pool.QueryRow(context.Background(), `select last_ping_at from kontra.tunnels where session_id = $1`, n.session).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if len(stamps) == 0 || !at.Equal(stamps[len(stamps)-1]) {
			stamps = append(stamps, at)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(stamps) < 4 || len(stamps) > 8 {
		t.Fatalf("last_ping_at took %d values in %v (want about one per %v): %v", len(stamps), 6*every, every, stamps)
	}
	for i := 2; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < every*3/4 {
			t.Fatalf("last_ping_at moved after %v, less than LastPingEvery (%v)", gap, every)
		}
	}

	// A row of this replica for a session it no longer holds (a close whose delete failed) is deleted by the
	// row check once it has gone two rounds without a last_ping_at; a fresh one is left (a registration in
	// progress), and so are other replicas' rows.
	stale, fresh := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	other := "r-other-" + uuid.NewString()[:8]
	if _, err := e.pool.Exec(context.Background(), `insert into kontra.gate_replicas (replica_id, version) values ($1, 'test')`, other); err != nil {
		t.Fatal(err)
	}
	foreign := uuid.Must(uuid.NewV7())
	for _, row := range []struct {
		id      uuid.UUID
		replica string
		age     string
	}{{stale, a.id, "1 hour"}, {fresh, a.id, "-1 minute"}, {foreign, other, "1 hour"}} { // fresh: a row the check cannot find stale during the test
		if _, err := e.pool.Exec(context.Background(), `insert into kontra.tunnels (session_id, key_id, tenant_id, replica_id, services, cli_version, last_ping_at)
			values ($1, $2, $3, $4, '{hello}', 'test', now() - $5::interval)`, row.id, id.KeyID(), e.team, row.replica, row.age); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 3*every, "the row check deletes the leftover row", func() bool { return !e.hasRow(t, stale) })
	if !e.hasRow(t, fresh) || !e.hasRow(t, foreign) || !e.hasRow(t, n.session) {
		t.Fatal("the row check deleted a fresh row, another replica's or a held session's")
	}

	if err := s.Close("test"); err != nil {
		t.Fatal(err)
	}
	if e.hasRow(t, n.session) {
		t.Fatal("Close left the tunnels row")
	}
	if !n.closed(5 * time.Second) {
		t.Fatal("the node's session stayed open")
	}
	if _, ok := a.reg.Get(n.session); ok {
		t.Fatal("Get returns a closed session")
	}
	if err := s.Allows("hello", tunnel.KindA2A); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("Allows on a closed session: %v", err)
	}
	eventually(t, 5*time.Second, "the registry forgets the session", func() bool { return a.reg.Count() == 0 })
}

// One session per key per gate (§11): a second handshake of the key on the same replica closes the first,
// whose open stream and every new one fail ErrSessionGone; the first's row is deleted, the second's kept.
func TestSupersedeOnOneReplica(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{})
	id := newIdentity(t)
	ag := newAgent()
	first := e.enrollNode(t, a, id, ag, "hello")
	old := a.held(t, first)
	body := slowExchange(t, old, "hello", ag)

	second := e.connectNode(t, a, id, ag, "hello")
	readsGone(t, body, 5*time.Second)
	if !first.closed(5 * time.Second) {
		t.Fatal("the superseded session stayed open")
	}
	if _, err := old.OpenExchange(context.Background(), tunnel.Preamble{Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindA2A}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("a stream on the superseded session: %v", err)
	}
	if e.hasRow(t, first.session) || !e.hasRow(t, second.session) {
		t.Fatal("rows after the supersede: want only the second session's")
	}
	if _, ok := a.reg.Get(second.session); !ok {
		t.Fatal("the new session is not held")
	}
	if !strings.Contains(a.logs.String(), "superseded by session "+second.session.String()) {
		t.Fatal("the supersede is not logged with the session that superseded it")
	}
}

// Across replicas (§11; the gate-internal NOTIFY of the confirmed spec conflict): the replica holding the
// superseded session closes it within 1 s of the NOTIFY; with the NOTIFY dropped, at its next row check.
// When the old session's replica is draining, both sessions stay (make-before-break, §13).
func TestSupersedeAcrossReplicas(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	const every = 500 * time.Millisecond
	a := e.replica("a", replicaOpts{every: time.Hour})
	b := e.replica("b", replicaOpts{every: every})
	id := newIdentity(t)
	ag := newAgent()
	onA := e.enrollNode(t, a, id, ag, "hello")

	// With the NOTIFY: A closes within 1 s of B's commit.
	onB := e.connectNode(t, b, id, ag, "hello")
	if !onA.closed(time.Second) {
		t.Fatal("A did not close its superseded session within 1 s of the NOTIFY")
	}
	eventually(t, time.Second, "A forgets the session", func() bool { _, ok := a.reg.Get(onA.session); return !ok })

	// Without it: B's session is superseded from A and B learns it at its row check.
	b.tunnelNotify.Store(false)
	start := time.Now()
	onA2 := e.connectNode(t, a, id, ag, "hello")
	if !onB.closed(every + 2*time.Second) {
		t.Fatal("B did not close its superseded session at its row check")
	}
	if took := time.Since(start); took > every+2*time.Second {
		t.Fatalf("B closed it after %v", took)
	}
	if !strings.Contains(b.logs.String(), "tunnel session's row is gone") {
		t.Fatal("B's close was not the row check's")
	}

	// A draining: the session on A stays when the key connects on B, and both rows are kept.
	if _, err := e.pool.Exec(context.Background(), `update kontra.gate_replicas set draining_at = now() where replica_id = $1`, a.id); err != nil {
		t.Fatal(err)
	}
	b.tunnelNotify.Store(true)
	onB2 := e.connectNode(t, b, id, ag, "hello")
	if onA2.closed(every + time.Second) {
		t.Fatal("the session on the draining replica was superseded")
	}
	if !e.hasRow(t, onA2.session) || !e.hasRow(t, onB2.session) {
		t.Fatal("make-before-break: both rows should stay")
	}
	if _, ok := a.reg.Get(onA2.session); !ok {
		t.Fatal("A dropped its session while draining")
	}
}

// Add's transaction locks the key's tunnel_keys row (M2 plan audit): a key whose row another transaction
// holds waits for it, and of many concurrent registrations of one key exactly one row remains.
func TestAddSerializesOneKey(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{})
	id := newIdentity(t)
	n := e.enrollNode(t, a, id, newAgent(), "hello")
	ctx := context.Background()

	session := func() *Session {
		return &Session{ID: uuid.Must(uuid.NewV7()), KeyID: id.KeyID(), TenantID: e.team, CLIVersion: "test",
			served: map[string]string{"hello": tunnel.KindA2A}}
	}
	// Another transaction holds the key's row: the insert waits for it.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `select 1 from kontra.tunnel_keys where key_id = $1 for update`, id.KeyID()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	s1 := session()
	go func() { _, err := a.reg.insert(ctx, s1); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("the insert did not wait for the key's row lock: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Concurrent registrations, committed in any order: one row stays, the newest handshake's (the highest
	// session id, a UUIDv7); a registration that finds a newer session's row is refused as superseded.
	sessions := make([]*Session, 8)
	for i := range sessions {
		sessions[i] = session()
	}
	newest := sessions[len(sessions)-1]
	var wg sync.WaitGroup
	for i := len(sessions) - 1; i >= 0; i-- { // the newest first: every older one then finds it
		wg.Go(func() {
			if _, err := a.reg.insert(ctx, sessions[i]); err != nil && !errors.Is(err, ErrSessionGone) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var rows []uuid.UUID
	r, err := e.pool.Query(ctx, `select session_id from kontra.tunnels where key_id = $1`, id.KeyID())
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var sid uuid.UUID
		if err := r.Scan(&sid); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, sid)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0] != newest.ID {
		t.Fatalf("tunnels rows %v for one key after concurrent registrations, want the newest handshake's (%s)", rows, newest.ID)
	}
	if e.hasRow(t, n.session) || e.hasRow(t, s1.ID) {
		t.Fatal("an older session of the key kept its row")
	}
}

// RevokeKey (§12): the node's control stream reads {"t":"revoked"} and its session closes, within 1 s with
// the NOTIFY, within 5 s without it (the view's re-read); the row is deleted, the next handshake is revoked.
func TestKeyRevocation(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{})
	ctx := context.Background()

	for _, notify := range []bool{true, false} {
		a.revokeNotify.Store(notify)
		id := newIdentity(t)
		ag := newAgent()
		n := e.enrollNode(t, a, id, ag, "hello")
		s := a.held(t, n)
		body := slowExchange(t, s, "hello", ag)
		start := time.Now()
		if err := e.store.RevokeKey(ctx, id.KeyID(), enroll.ReasonOperator, "test"); err != nil {
			t.Fatal(err)
		}
		within := time.Second
		if !notify {
			within = enroll.RevocationRefresh
		}
		if c := n.nextControl(t, within); c.T != tunnel.CtlRevoked || c.Service != "" {
			t.Fatalf("control %+v, want {t: revoked}", c)
		}
		if !n.closed(within - time.Since(start) + 200*time.Millisecond) {
			t.Fatalf("the revoked key's session is open %v after the revocation (NOTIFY %v)", time.Since(start), notify)
		}
		readsGone(t, body, time.Second)
		if err := s.Allows("hello", tunnel.KindA2A); !errors.Is(err, ErrSessionGone) {
			t.Fatalf("Allows after the revocation: %v", err)
		}
		eventually(t, 2*time.Second, "the revoked session's row is deleted", func() bool { return !e.hasRow(t, n.session) })
		if _, err := a.startNode(t, id, newHello("hello"), ag); handshakeCode(t, err) != tunnel.CodeRevoked {
			t.Fatalf("the next handshake: %v, want revoked", err)
		}
	}
}

// RevokeBinding (§12): Allows refuses the service, tunnels.services drops it, an in-flight stream for it is
// aborted (ErrSessionGone: the exchange reads as a lost session), the node reads {"t":"revoked","service"},
// the mcp_sessions rows pinned to (session, service) close revoked, and the session's other service is
// unaffected, its in-flight stream included.
func TestBindingRevocation(t *testing.T) {
	e := newTunnelEnv(t, "hello", "recon")
	a := e.replica("a", replicaOpts{})
	ctx := context.Background()
	id := newIdentity(t)
	ag := newAgent()
	n := e.enrollNode(t, a, id, ag, "hello", "recon")
	s := a.held(t, n)
	for _, svc := range []string{"hello", "recon"} {
		if _, err := e.pool.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via,
			tunnel_session, replica_id, protocol_version) values (sha256($1::bytea), $2, $3, $2, 'key:x', 'api_key', $4, $5, '2025-11-25')`,
			[]byte(svc), e.team, svc, n.session, a.id); err != nil {
			t.Fatal(err)
		}
	}
	hello := slowExchange(t, s, "hello", ag)
	recon := slowExchange(t, s, "recon", ag)

	if err := e.store.RevokeBinding(ctx, id.KeyID(), "hello", "test"); err != nil {
		t.Fatal(err)
	}
	if c := n.nextControl(t, time.Second); c.T != tunnel.CtlRevoked || c.Service != "hello" {
		t.Fatalf("control %+v, want {t: revoked, service: hello}", c)
	}
	readsGone(t, hello, time.Second)
	if err := s.Allows("hello", tunnel.KindA2A); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("Allows(hello) after its binding was revoked: %v", err)
	}
	if _, err := s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindA2A}); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("a new stream for hello: %v", err)
	}
	if got := e.rowServices(t, n.session); !slices.Equal(got, []string{"recon"}) {
		t.Fatalf("tunnels.services %v, want [recon]", got)
	}
	closed := map[string]string{}
	rows, err := e.pool.Query(ctx, `select service, coalesce(close_reason, '') from kontra.mcp_sessions where tunnel_session = $1`, n.session)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var svc, reason string
		if err := rows.Scan(&svc, &reason); err != nil {
			t.Fatal(err)
		}
		closed[svc] = reason
	}
	if closed["hello"] != "revoked" || closed["recon"] != "" {
		t.Fatalf("mcp_sessions close reasons %v", closed)
	}

	// recon is unaffected.
	if err := s.Allows("recon", tunnel.KindA2A); err != nil {
		t.Fatalf("Allows(recon): %v", err)
	}
	close(ag.release)
	rest, err := io.ReadAll(recon)
	if err != nil || string(rest) != "data: last\n\n" {
		t.Fatalf("recon's in-flight stream: %q, %v", rest, err)
	}
	if n.closed(200 * time.Millisecond) {
		t.Fatal("a binding revocation closed the session")
	}
	if _, ok := a.reg.Get(n.session); !ok {
		t.Fatal("the session is no longer held")
	}
}

// Allows (relay.md §6.1, key_scope) refuses a service the session does not serve and another kind; it
// accepts a rotated key inside its overlap on its existing session and refuses it past overlap_until (before
// the sweep revokes it); when the overlap is ended by the sweep the session closes without {"t":"revoked"}.
// While the revocation view is not fresh, the rows are read, and an operator's revocation is refused.
func TestAllowsRefusals(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{})
	ctx := context.Background()
	id := newIdentity(t)
	n := e.enrollNode(t, a, id, newAgent(), "hello")
	s := a.held(t, n)
	for _, tc := range []struct{ service, kind string }{{"nope", tunnel.KindA2A}, {"hello", tunnel.KindMCP}} {
		if err := s.Allows(tc.service, tc.kind); !errors.Is(err, ErrSessionGone) {
			t.Fatalf("Allows(%s, %s) = %v", tc.service, tc.kind, err)
		}
	}
	if err := s.Allows("hello", tunnel.KindA2A); err != nil {
		t.Fatal(err)
	}

	// Rotation (§12): the new key connects with rotate_from; the old key's session keeps serving.
	nid, err := nodekey.Rotate(id.Path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := newHello("hello")
	h.RotateFrom = &tunnel.KeyRef{KID: id.KeyID(), Recipient: id.Recipient()}
	rotated, err := a.startNode(t, nid, h, newAgent())
	if err != nil {
		t.Fatalf("rotation: %v", err)
	}
	e.waitRow(t, rotated.session)
	if !a.rev.Fresh() {
		t.Fatal("the view is not fresh")
	}
	time.Sleep(enroll.RevocationRefresh*3/5 + 500*time.Millisecond) // a re-read holds the rotated key in its overlap
	if err := s.Allows("hello", tunnel.KindA2A); err != nil {
		t.Fatalf("the rotated key inside its overlap: %v", err)
	}
	// Past overlap_until, before the sweep: refused.
	if _, err := e.pool.Exec(ctx, `update kontra.tunnel_keys set overlap_until = now() - interval '1 second' where key_id = $1`, id.KeyID()); err != nil {
		t.Fatal(err)
	}
	eventually(t, enroll.RevocationRefresh, "Allows refuses the key past its overlap", func() bool {
		return errors.Is(s.Allows("hello", tunnel.KindA2A), ErrSessionGone)
	})
	if n.closed(0) {
		t.Fatal("the session closed before the sweep revoked the key")
	}
	// The sweep ends the overlap: the session closes, without {"t": "revoked"} (the node moved to its new
	// key); the new key's session is unaffected.
	if ended, err := e.store.EndOverlaps(ctx); err != nil || !slices.Contains(ended, id.KeyID()) {
		t.Fatalf("EndOverlaps = %v, %v", ended, err)
	}
	if !n.closed(2 * time.Second) {
		t.Fatal("the session of a key whose overlap ended stayed open")
	}
	n.noControl(t, 200*time.Millisecond)
	if _, ok := a.reg.Get(rotated.session); !ok {
		t.Fatal("the new key's session was closed")
	}

	// A view that is not fresh: the rows are read.
	other := newIdentity(t)
	on := e.enrollNode(t, a, other, newAgent(), "hello")
	stale := &Session{ID: on.session, KeyID: other.KeyID(), TenantID: e.team, served: map[string]string{"hello": tunnel.KindA2A},
		reg: &Registry{cfg: RegistryConfig{Revocations: enroll.NewRevocations(e.pool)}}}
	if err := stale.Allows("hello", tunnel.KindA2A); err != nil {
		t.Fatalf("an enrolled key through the rows: %v", err)
	}
	if err := e.store.RevokeBinding(ctx, other.KeyID(), "hello", "test"); err != nil {
		t.Fatal(err)
	}
	if err := stale.Allows("hello", tunnel.KindA2A); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("a revoked binding through the rows: %v", err)
	}
}

// Enrolling a second key for a hosted service tells the first key's session {"t":"enrolled","service",
// "key"} (§5), on the same replica (at once) and on another one (NOTIFY kontra_tunnel), once each; the
// enrolled key itself is not told.
func TestEnrolledNotices(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{})
	b := e.replica("b", replicaOpts{})
	first := e.enrollNode(t, a, newIdentity(t), newAgent(), "hello")

	k2 := newIdentity(t)
	second := e.enrollNode(t, a, k2, newAgent(), "hello")
	if c := first.nextControl(t, 2*time.Second); c != (tunnel.Control{T: tunnel.CtlEnrolled, Service: "hello", Key: k2.KeyID()}) {
		t.Fatalf("control %+v, want enrolled hello %s", c, k2.KeyID())
	}
	first.noControl(t, 500*time.Millisecond) // the replica's own NOTIFY does not tell it twice
	second.noControl(t, 0)

	k3 := newIdentity(t)
	third := e.enrollNode(t, b, k3, newAgent(), "hello")
	for _, n := range []*testNode{first, second} {
		if c := n.nextControl(t, 2*time.Second); c != (tunnel.Control{T: tunnel.CtlEnrolled, Service: "hello", Key: k3.KeyID()}) {
			t.Fatalf("control %+v, want enrolled hello %s", c, k3.KeyID())
		}
		n.noControl(t, 300*time.Millisecond)
	}
	third.noControl(t, 300*time.Millisecond)
}

// Drain (§13, D35): every session reads {"t":"drain"} and stays open; the Handler answers new upgrades
// draining; CloseAll closes the sessions, deletes their rows and refuses later registrations.
func TestDrainAndCloseAll(t *testing.T) {
	e := newTunnelEnv(t, "hello", "recon")
	a := e.replica("a", replicaOpts{})
	n1 := e.enrollNode(t, a, newIdentity(t), newAgent(), "hello")
	n2 := e.enrollNode(t, a, newIdentity(t), newAgent(), "recon")

	a.draining.Store(true)
	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	a.reg.Drain(dctx)
	cancel()
	for _, n := range []*testNode{n1, n2} {
		if c := n.nextControl(t, time.Second); c.T != tunnel.CtlDrain {
			t.Fatalf("control %+v, want drain", c)
		}
		if n.closed(0) {
			t.Fatal("drain closed a session")
		}
	}
	if _, err := a.startNode(t, newIdentity(t), newHello("hello"), newAgent()); handshakeCode(t, err) != tunnel.CodeDraining {
		t.Fatalf("a new tunnel on a draining replica: %v", err)
	}
	// Drain is told once per session.
	a.reg.Drain(context.Background())
	n1.noControl(t, 200*time.Millisecond)

	a.reg.CloseAll(context.Background())
	for _, n := range []*testNode{n1, n2} {
		if !n.closed(5 * time.Second) {
			t.Fatal("CloseAll left a session open")
		}
		if e.hasRow(t, n.session) {
			t.Fatal("CloseAll left a tunnels row")
		}
	}
	if a.reg.Count() != 0 {
		t.Fatalf("%d sessions after CloseAll", a.reg.Count())
	}
	late := &Session{ID: uuid.Must(uuid.NewV7()), KeyID: "k1_" + strings.Repeat("a", 26), served: map[string]string{}}
	_, gm := tunneltest.NewPair(t).Sessions(t, tunnel.YamuxConfig(nil))
	if err := a.reg.Add(context.Background(), late, gm, nil); !errors.Is(err, errClosing) {
		t.Fatalf("Add after CloseAll: %v", err)
	}
}

// A tunnel service's card (relay.md §3): fetched through a session as two exchanges whose preamble names
// the service without a task (§8.1), the newest session first; OnConnect runs before the session's row
// exists (it is no candidate yet, M2 plan audit gap 3); with no session here FetchCard is ErrSessionGone.
func TestFetchCardThroughSession(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{})
	ctx := context.Background()
	if _, err := a.reg.FetchCard(ctx, e.team, "hello"); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("FetchCard with no session: %v", err)
	}
	type connectSeen struct {
		service string
		row     bool
		card    string
	}
	seen := make(chan connectSeen, 8)
	a.reg.OnConnect(func(ctx context.Context, tid uuid.UUID, service string) {
		var row bool
		_ = e.pool.QueryRow(ctx, `select exists (select 1 from kontra.tunnels where tenant_id = $1 and $2 = any(services))`, tid, service).Scan(&row)
		f, err := a.reg.FetchCard(ctx, tid, service)
		c := ""
		if err == nil {
			c = string(f.Raw)
		}
		seen <- connectSeen{service, row, c}
	})
	old := newAgent()
	old.card = strings.Replace(testCard, `"version":"1.0.0"`, `"version":"0.9.0"`, 1)
	n1 := e.enrollNode(t, a, newIdentity(t), old, "hello")
	if s := <-seen; s.service != "hello" || s.row || s.card != old.card {
		t.Fatalf("OnConnect saw %+v", s)
	}
	if p := <-n1.preambles; p != (tunnel.Preamble{V: 1, Op: tunnel.OpHTTP, Service: "hello", Kind: tunnel.KindA2A}) {
		t.Fatalf("the card fetch's preamble %+v", p)
	}
	newer := newAgent()
	n2 := e.enrollNode(t, a, newIdentity(t), newer, "hello")
	<-seen
	f, err := a.reg.FetchCard(ctx, e.team, "hello")
	if err != nil || string(f.Raw) != testCard || f.Path != "/.well-known/agent-card.json" {
		t.Fatalf("FetchCard = %v, %v; want the newest session's card", f, err)
	}
	if newer.gets() != 2 || old.gets() != 1 {
		t.Fatalf("card GETs: newest %d, oldest %d", newer.gets(), old.gets())
	}
	if !a.reg.Serves(e.team, "hello") || a.reg.Serves(e.team, "other") {
		t.Fatal("Serves")
	}
	// The newest session gone, the older one serves the fetch.
	if s, ok := a.reg.Get(n2.session); !ok || s.Close("test") != nil {
		t.Fatal("close the newest session")
	}
	if f, err := a.reg.FetchCard(ctx, e.team, "hello"); err != nil || string(f.Raw) != old.card {
		t.Fatalf("FetchCard after the newest closed = %v, %v", f, err)
	}
}

// A key's sessions close when its tenant is deleted (M2 plan, WP-04 note): the tunnel_keys and tunnels rows
// cascade, and the replica's row check closes the session whose row is gone.
func TestTenantDeletionClosesSessions(t *testing.T) {
	e := newTunnelEnv(t)
	ctx := context.Background()
	acme := uuid.Must(uuid.NewV7())
	if _, err := e.pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind) values ($1, 'acme', 'account')`, acme); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source)
		values ($1, 'hello', 'a2a', '{"tunnel": {}}'::jsonb, 'db')`, acme); err != nil {
		t.Fatal(err)
	}
	const every = 300 * time.Millisecond
	a := e.replica("a", replicaOpts{every: every})
	tok, err := e.store.MintServiceToken(ctx, acme, "hello", "test")
	if err != nil {
		t.Fatal(err)
	}
	h := newHello("hello")
	h.Enroll = []string{tok}
	n, err := a.startNode(t, newIdentity(t), h, newAgent())
	if err != nil {
		t.Fatal(err)
	}
	e.waitRow(t, n.session)
	if _, err := e.pool.Exec(ctx, `delete from kontra.tenants where id = $1`, acme); err != nil {
		t.Fatal(err)
	}
	if !n.closed(every + 2*time.Second) {
		t.Fatal("the session of a deleted tenant's key stayed open")
	}
	if _, ok := a.reg.Get(n.session); ok {
		t.Fatal("the registry still holds it")
	}
}

// connectHook is an OnConnect callback a test arms for the next session that connects: fn is called with
// the session being registered (the newest one not registered yet), during its card fetch.
type connectHook struct {
	mu   sync.Mutex
	next func(ctx context.Context, s *Session)
}

func hookConnect(r *Registry) *connectHook {
	h := &connectHook{}
	r.OnConnect(func(ctx context.Context, tid uuid.UUID, service string) {
		h.mu.Lock()
		fn := h.next
		h.next = nil
		h.mu.Unlock()
		if fn == nil {
			return
		}
		for _, s := range r.ForService(tid, service) {
			s.mu.Lock()
			pending := !s.registered
			s.mu.Unlock()
			if pending {
				fn(ctx, s)
				return
			}
		}
	})
	return h
}

func (h *connectHook) arm(fn func(ctx context.Context, s *Session)) {
	h.mu.Lock()
	h.next = fn
	h.mu.Unlock()
}

// returned waits for one of the replica's handshakes (ServeHTTP) to return.
func (r *replica) returned(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-r.gate.returned:
	case <-time.After(within):
		t.Fatalf("no tunnel handler returned within %v", within)
	}
}

// One session per key per gate (§11) follows the order of the handshakes, not of their commits (M2 review of
// WP-04): an older handshake whose card fetch outlasts a newer one's (a lost welcome's redial while the old
// connection is still being registered, M2 plan audit gap 1) finds the newer session registered, inserts
// nothing and closes; the newer session keeps its row and stays open.
func TestOlderHandshakeDoesNotSupersedeNewer(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{cardWait: 10 * time.Second})
	hook := hookConnect(a.reg)
	id := newIdentity(t)
	ag := newAgent()
	first := e.enrollNode(t, a, id, ag, "hello")

	fetching, release := make(chan uuid.UUID, 1), make(chan struct{})
	hook.arm(func(ctx context.Context, s *Session) {
		fetching <- s.ID
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	older, err := a.startNode(t, id, newHello("hello"), ag)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fetching:
		if got != older.session {
			t.Fatalf("the held card fetch is session %s's, want %s", got, older.session)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the older session's card fetch did not start")
	}
	newer := e.connectNode(t, a, id, ag, "hello")
	if !first.closed(5 * time.Second) {
		t.Fatal("the newer session did not supersede the first")
	}
	close(release)
	if !older.closed(5 * time.Second) {
		t.Fatal("the older handshake's session stayed open although a newer one of its key is registered")
	}
	if newer.closed(500 * time.Millisecond) {
		t.Fatal("the older handshake closed the newer session")
	}
	if !e.hasRow(t, newer.session) || e.hasRow(t, older.session) || e.hasRow(t, first.session) {
		t.Fatalf("rows: newer %v, older %v, first %v; want only the newer's", e.hasRow(t, newer.session), e.hasRow(t, older.session), e.hasRow(t, first.session))
	}
	if _, ok := a.reg.Get(newer.session); !ok {
		t.Fatal("the newer session is not held")
	}
	if _, ok := a.reg.Get(older.session); ok {
		t.Fatal("the older session is held")
	}
	if !strings.Contains(a.gate.logs.String(), "newer session "+newer.session.String()) {
		t.Fatal("the older session's refusal does not name the newer session")
	}
}

// A session that closed before its row went in (its connection cut while the gate fetched its card)
// supersedes nothing (M2 review of WP-04): the key's registered session keeps its row and stays open.
func TestClosedSessionSupersedesNothing(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{})
	hook := hookConnect(a.reg)
	id := newIdentity(t)
	ag := newAgent()
	first := e.enrollNode(t, a, id, ag, "hello")

	hook.arm(func(_ context.Context, s *Session) { _ = s.mux.Close() })
	cut, err := a.startNode(t, id, newHello("hello"), ag)
	if err != nil {
		t.Fatal(err)
	}
	a.returned(t, 10*time.Second) // the cut session's Add has ended
	if !cut.closed(5 * time.Second) {
		t.Fatal("the cut session stayed open")
	}
	if e.hasRow(t, cut.session) || !e.hasRow(t, first.session) {
		t.Fatalf("rows: cut %v, first %v; want only the first's", e.hasRow(t, cut.session), e.hasRow(t, first.session))
	}
	if _, ok := a.reg.Get(first.session); !ok || first.closed(200*time.Millisecond) {
		t.Fatal("a closed session's registration closed the key's live session")
	}
}

// A session that closes while its row goes in still closes the sessions its commit superseded (M2 review of
// WP-04): their rows are gone, so they must not stay reachable here until the next row check.
func TestSupersededCloseWhenTheNewSessionClosesAtCommit(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{every: time.Hour})
	id := newIdentity(t)
	ag := newAgent()
	first := e.enrollNode(t, a, id, ag, "hello")
	ctx := context.Background()

	// The first session's row is held: the new session's transaction waits on it, after its checks.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select 1 from kontra.tunnels where session_id = $1 for update`, first.session); err != nil {
		t.Fatal(err)
	}
	second, err := a.startNode(t, id, newHello("hello"), ag)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the new session's transaction waits on the first session's row", func() bool {
		var n int
		if err := e.pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like '%delete from kontra.tunnels t%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	})
	_ = second.mux.Close() // the node leaves while its row goes in
	eventually(t, 5*time.Second, "the replica sees the new session closed", func() bool { return a.reg.lookup(second.session) == nil })
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if !first.closed(5 * time.Second) {
		t.Fatal("the superseded session stayed open after the commit that deleted its row")
	}
	if _, ok := a.reg.Get(first.session); ok {
		t.Fatal("the superseded session is still held")
	}
	eventually(t, 5*time.Second, "no row of the key remains", func() bool { return !e.hasRow(t, first.session) && !e.hasRow(t, second.session) })
}

// The row check deletes this replica's leftover rows also when it holds no session (M2 review of WP-04):
// the commonest case, the replica's only session closed during a database blip and its delete failed. An
// empty set of held sessions must not read as NULL, which matches nothing.
func TestRowCheckWithNoSessionHeld(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	const every = 300 * time.Millisecond
	a := e.replica("a", replicaOpts{every: every})
	id := newIdentity(t)
	n := e.enrollNode(t, a, id, newAgent(), "hello")
	if err := a.held(t, n).Close("test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the registry holds no session", func() bool { return a.reg.Count() == 0 })
	stale := uuid.Must(uuid.NewV7())
	if _, err := e.pool.Exec(context.Background(), `insert into kontra.tunnels (session_id, key_id, tenant_id, replica_id, services, cli_version, last_ping_at)
		values ($1, $2, $3, $4, '{hello}', 'test', now() - interval '1 hour')`, stale, id.KeyID(), e.team, a.id); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*every+2*time.Second, "the row check deletes the leftover row of a replica holding no session", func() bool { return !e.hasRow(t, stale) })
}

// The {"t":"enrolled"} notice (§5) follows the enrollment's commit (Admit's NOTIFY kontra_tunnel), not the
// new key's registration (M2 review of WP-04): the first key's node, on the same replica, is told even when
// the new key's session closes before its row goes in, and told once.
func TestEnrolledNoticeWhenTheNewSessionCloses(t *testing.T) {
	e := newTunnelEnv(t, "hello")
	a := e.replica("a", replicaOpts{})
	hook := hookConnect(a.reg)
	ctx := context.Background()
	first := e.enrollNode(t, a, newIdentity(t), newAgent(), "hello")

	hook.arm(func(_ context.Context, s *Session) { _ = s.mux.Close() })
	k2 := newIdentity(t)
	tok, err := e.store.MintServiceToken(ctx, e.team, "hello", "test")
	if err != nil {
		t.Fatal(err)
	}
	h := newHello("hello")
	h.Enroll = []string{tok}
	second, err := a.startNode(t, k2, h, newAgent())
	if err != nil {
		t.Fatal(err)
	}
	if !second.closed(5 * time.Second) {
		t.Fatal("the new key's session stayed open")
	}
	var bound bool
	if err := e.pool.QueryRow(ctx, `select exists (select 1 from kontra.key_services where key_id = $1 and service = 'hello' and revoked_at is null)`,
		k2.KeyID()).Scan(&bound); err != nil || !bound {
		t.Fatalf("the new key's binding: %v, %v", bound, err)
	}
	if c := first.nextControl(t, 2*time.Second); c != (tunnel.Control{T: tunnel.CtlEnrolled, Service: "hello", Key: k2.KeyID()}) {
		t.Fatalf("control %+v, want enrolled hello %s", c, k2.KeyID())
	}
	first.noControl(t, 500*time.Millisecond)
}
