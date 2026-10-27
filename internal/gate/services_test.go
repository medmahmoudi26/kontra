package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/a2ahttp"
	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/upstream"
)

// parseConfig loads a kontra.yaml document with the fields every test needs filled in.
func parseConfig(t *testing.T, services string) *config.Config {
	t.Helper()
	doc := "public_url: http://localhost:8080\npostgres: postgres://kontra@localhost/kontra\nauth: {api_keys: true}\n" + services
	cfg, err := config.Parse([]byte(doc), "")
	if err != nil {
		t.Fatalf("config: %v\n%s", err, doc)
	}
	return cfg
}

type auditRow struct {
	Event   string   `json:"event"`
	Service string   `json:"service"`
	Changes []string `json:"changes"`
	Allow   []string `json:"allow"`
}

// auditEvents returns the audit rows after the first skip ones, checking the whole chain.
func auditEvents(t *testing.T, pool *pgxpool.Pool, skip int) []auditRow {
	t.Helper()
	rows, err := audit.Verify(context.Background(), pool)
	if err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	var out []auditRow
	for _, r := range rows[min(skip, len(rows)):] {
		var a auditRow
		if err := json.Unmarshal(r.RowJCS, &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// notifyRecorder collects NOTIFY kontra_services payloads on a connection of its own.
type notifyRecorder struct {
	mu  sync.Mutex
	got []string
}

func recordNotifies(t *testing.T, dsn string) *notifyRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "listen kontra_services"); err != nil {
		t.Fatal(err)
	}
	n := &notifyRecorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			nt, err := conn.WaitForNotification(ctx)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.got = append(n.got, nt.Payload)
			n.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		conn.Close(context.Background())
	})
	return n
}

func (n *notifyRecorder) waitFor(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n.mu.Lock()
		got := slices.Clone(n.got)
		n.mu.Unlock()
		ok := true
		for _, w := range want {
			if !slices.Contains(got, w) {
				ok = false
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("NOTIFY kontra_services: got %v, want %v", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (n *notifyRecorder) reset() {
	n.mu.Lock()
	n.got = nil
	n.mu.Unlock()
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

const syncServicesA = `services:
  hello:
    kind: a2a
    backend: { a2a: { url: "http://127.0.0.1:9/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1, retention: 2d }
  tool:
    kind: a2a
    backend: { tunnel: {} }
    allow: [anonymous]
  files:
    kind: mcp
    backend: { mcp: { url: "http://127.0.0.1:9/mcp", allow_private: true } }
    allow: ["org:default"]
    tools: { allow: ["read_*"] }
`

// Sync writes the team tenant and the services: block (source config), appends one audit row per created,
// changed or disabled service and NOTIFYs it; a Sync of the same file writes nothing (spec §13.5; the
// audit gap's WP-08 half).
func TestSyncWritesTheBlockAndAuditsChanges(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	notes := recordNotifies(t, dsn)
	var changedMu sync.Mutex
	var changed []string
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", OnChange: func(ref store.ServiceRef) {
		changedMu.Lock()
		changed = append(changed, ref.Name)
		changedMu.Unlock()
	}})

	if err := s.Sync(ctx, parseConfig(t, syncServicesA)); err != nil {
		t.Fatal(err)
	}
	if kind := scalar[string](t, pool, `select kind from kontra.tenants where slug = 'default'`); kind != "team" {
		t.Fatalf("tenant default is %s", kind)
	}
	var (
		backend, annots, tasksMode string
		tools                      *string
		allow                      []string
		timeoutS, retentionS       int
		retries                    int
		source                     string
	)
	if err := pool.QueryRow(ctx, `select backend::text, annotations::text, coalesce(tasks_mode, ''), tools::text, allow, timeout_s, retention_s, retries, source
		from kontra.services where name = 'hello'`).Scan(&backend, &annots, &tasksMode, &tools, &allow, &timeoutS, &retentionS, &retries, &source); err != nil {
		t.Fatal(err)
	}
	if backend != `{"a2a": {"url": "http://127.0.0.1:9/base", "allow_private": true}}` || annots != "{}" || tasksMode != "" || tools != nil ||
		!slices.Equal(allow, []string{"org:default"}) || timeoutS != 600 || retentionS != 2*86400 || retries != 1 || source != "config" {
		t.Fatalf("hello row: %s %s %q %v %v %d %d %d %s", backend, annots, tasksMode, tools, allow, timeoutS, retentionS, retries, source)
	}
	if got := scalar[string](t, pool, `select tasks_mode || ' ' || tools::text from kontra.services where name = 'files'`); got != `durable {"deny": [], "allow": ["read_*"]}` {
		t.Fatalf("files row: %s", got)
	}
	ev := auditEvents(t, pool, 0)
	if len(ev) != 3 {
		t.Fatalf("audit after the first sync: %+v", ev)
	}
	for _, a := range ev {
		if a.Event != EventServiceUpserted || !slices.Equal(a.Changes, []string{"created"}) {
			t.Fatalf("audit row %+v", a)
		}
	}
	notes.waitFor(t, "default/hello", "default/tool", "default/files")

	// The registry serves what the rows say.
	hello, _ := s.Lookup(ctx, TeamTenant, "hello")
	if hello == nil || hello.Disabled || hello.Ref.Timeout != 10*time.Minute || hello.Ref.Retries != 1 || hello.Ref.Retention != 48*time.Hour || hello.Ref.Kind != "a2a" {
		t.Fatalf("Lookup hello: %+v", hello)
	}
	tid := hello.Ref.TenantID
	// The dispatcher reads the service with LoadService and the A2A routes with Lookup: relay.Handlers
	// builds a handler again for a ServiceRef other than its cached one's, so both must read the same ref.
	if ref, err := store.NewLog(pool).LoadService(ctx, tid, "hello"); err != nil || ref != hello.Ref {
		t.Fatalf("LoadService = %+v, %v; Lookup = %+v", ref, err, hello.Ref)
	}
	member := auth.Principal{Kind: auth.KindKey, ID: "key:kk_a_00000001", BuyerID: uuid.New(), Via: auth.ViaAPIKey,
		Scopes: []auth.Scope{auth.ScopeHire}, TenantID: &tid, TenantSlug: TeamTenant}
	if !hello.Allow.Admits(member) || hello.Allow.Admits(auth.Anonymous(uuid.New())) {
		t.Fatal("hello's allow: list is not org:default")
	}
	if b, err := s.BackendFor(ctx, tid, "hello"); err != nil || b != store.BackendDirect {
		t.Fatalf("BackendFor hello = %q, %v", b, err)
	}
	if b, err := s.BackendFor(ctx, tid, "tool"); err != nil || b != store.BackendTunnel {
		t.Fatalf("BackendFor tool = %q, %v", b, err)
	}
	if _, _, err := s.Backend(ctx, hello.Ref, nil); !errors.Is(err, card.ErrUnavailable) {
		t.Fatalf("Backend of a service whose card was never fetched: %v", err)
	}
	if got, _ := s.Lookup(ctx, TeamTenant, "nope"); got != nil {
		t.Fatalf("Lookup of an unknown service: %+v", got)
	}

	// The same file again: nothing written, nothing audited.
	updated := scalar[time.Time](t, pool, `select max(updated_at) from kontra.services`)
	notes.reset()
	if err := s.Sync(ctx, parseConfig(t, syncServicesA)); err != nil {
		t.Fatal(err)
	}
	if n := len(auditEvents(t, pool, 0)); n != 3 {
		t.Fatalf("a sync of the same file appended audit rows: %d", n)
	}
	if u := scalar[time.Time](t, pool, `select max(updated_at) from kontra.services`); !u.Equal(updated) {
		t.Fatal("a sync of the same file rewrote a service")
	}

	// A changed allow: list and a removed service.
	changedMu.Lock()
	changed = nil
	changedMu.Unlock()
	b := strings.Replace(syncServicesA, `allow: ["org:default"]
    task:`, `allow: ["org:default", "key:kk_ci_*"]
    task:`, 1)
	b = strings.Replace(b, "  tool:\n    kind: a2a\n    backend: { tunnel: {} }\n    allow: [anonymous]\n", "", 1)
	if err := s.Sync(ctx, parseConfig(t, b)); err != nil {
		t.Fatal(err)
	}
	ev = auditEvents(t, pool, 3)
	if len(ev) != 2 || ev[0].Event != EventServiceUpserted || ev[0].Service != "hello" || !slices.Equal(ev[0].Changes, []string{"allow"}) ||
		!slices.Equal(ev[0].Allow, []string{"org:default", "key:kk_ci_*"}) || ev[1].Event != EventServiceDisabled || ev[1].Service != "tool" {
		t.Fatalf("audit after a changed allow: and a removed service: %+v", ev)
	}
	notes.waitFor(t, "default/hello", "default/tool")
	if !scalar[bool](t, pool, `select disabled_at is not null from kontra.services where name = 'tool'`) {
		t.Fatal("a service removed from the file has no disabled_at")
	}
	if tool, _ := s.Lookup(ctx, TeamTenant, "tool"); tool == nil || !tool.Disabled {
		t.Fatalf("Lookup of a removed service: %+v (want disabled, which the route answers 404)", tool)
	}
	if _, err := s.BackendFor(ctx, tid, "tool"); err == nil {
		t.Fatal("BackendFor a disabled service answered a backend")
	}
	hello, _ = s.Lookup(ctx, TeamTenant, "hello")
	ci := member
	ci.ID = "key:kk_ci_0000001x"
	ci.TenantID = nil
	if !hello.Allow.Admits(ci) {
		t.Fatal("the changed allow: list is not applied")
	}
	changedMu.Lock()
	if !slices.Contains(changed, "hello") || !slices.Contains(changed, "tool") || slices.Contains(changed, "files") {
		t.Errorf("OnChange saw %v, want hello and tool only", changed)
	}
	changedMu.Unlock()

	// Back in the file: enabled again. A changed URL drops the stored cards (another seller's).
	if _, err := pool.Exec(ctx, `update kontra.services set upstream_card = '\x7b7d', card = '{}', card_fetched_at = now() where name = 'hello'`); err != nil {
		t.Fatal(err)
	}
	c := strings.Replace(syncServicesA, "127.0.0.1:9/base", "127.0.0.1:10/base", 1)
	if err := s.Sync(ctx, parseConfig(t, c)); err != nil {
		t.Fatal(err)
	}
	ev = auditEvents(t, pool, 5)
	if len(ev) != 2 || ev[0].Service != "hello" || !slices.Equal(ev[0].Changes, []string{"backend", "allow"}) ||
		ev[1].Service != "tool" || !slices.Equal(ev[1].Changes, []string{"enabled"}) {
		t.Fatalf("audit after re-adding a service and changing a URL: %+v", ev)
	}
	if scalar[bool](t, pool, `select upstream_card is not null or card is not null or card_fetched_at is not null from kontra.services where name = 'hello'`) {
		t.Fatal("a changed backend kept the old seller's card")
	}
	if tool, _ := s.Lookup(ctx, TeamTenant, "tool"); tool == nil || tool.Disabled {
		t.Fatalf("Lookup of a re-added service: %+v", tool)
	}
}

// A services: entry never overwrites a service the operator's control plane writes (source db); the whole
// sync is refused and nothing of it is written.
func TestSyncRefusesADBSourcedService(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	tid := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`, tid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, 'hello', 'a2a', '{"tunnel": {}}', 'db')`, tid); err != nil {
		t.Fatal(err)
	}
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080"})
	err := s.Sync(ctx, parseConfig(t, syncServicesA))
	if !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("Sync over a db service: %v", err)
	}
	if n := scalar[int](t, pool, `select count(*) from kontra.services`); n != 1 {
		t.Fatalf("a refused sync wrote %d services", n)
	}
	if n := scalar[int](t, pool, `select count(*) from kontra.audit`); n != 0 {
		t.Fatalf("a refused sync appended %d audit rows", n)
	}
}

// A multi-tenant gate reads the operator's rows (spec §13.5) and refuses a services: block; a NOTIFY
// kontra_services applies one service, and a service whose tenant is deleted goes away.
func TestMultiTenantRegistryFollowsTheRows(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	acme := uuid.Must(uuid.NewV7())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'acme', 'account')`, acme)
	exec(`insert into kontra.services (tenant_id, name, kind, backend, allow, source) values ($1, 'quote', 'a2a', '{"tunnel": {}}', '{org:acme}', 'db')`, acme)
	var changed []string
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Multi: true, OnChange: func(ref store.ServiceRef) {
		changed = append(changed, ref.Tenant+"/"+ref.Name)
	}})
	multi := parseConfig(t, "tenancy: multi\n")
	if err := s.Sync(ctx, parseConfig(t, "tenancy: multi\n"+syncServicesA)); err == nil {
		t.Fatal("a multi-tenant gate accepted a services: block")
	}
	if err := s.Sync(ctx, multi); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, pool, `select count(*) from kontra.tenants where slug = 'default'`); n != 0 {
		t.Fatal("a multi-tenant gate created the team tenant")
	}
	if q, _ := s.Lookup(ctx, "acme", "quote"); q == nil || q.Ref.TenantID != acme {
		t.Fatalf("Lookup acme/quote: %+v", q)
	}
	if got := ServicePath("acme", "quote", true); got != "/agents/acme/quote" {
		t.Fatalf("ServicePath = %s", got)
	}

	// A new row and a changed one, each announced by NOTIFY.
	exec(`insert into kontra.services (tenant_id, name, kind, backend, allow, source) values ($1, 'book', 'a2a', '{"tunnel": {}}', '{anonymous}', 'db')`, acme)
	s.Notify("acme/book")
	if b, _ := s.Lookup(ctx, "acme", "book"); b == nil || !b.Allow.AdmitsAnonymous() {
		t.Fatalf("Lookup acme/book after its NOTIFY: %+v", b)
	}
	exec(`update kontra.services set listed = true, retries = 5 where name = 'quote'`)
	s.Notify("acme/quote")
	if q, _ := s.Lookup(ctx, "acme", "quote"); q == nil || !q.Listed || q.Ref.Retries != 5 {
		t.Fatalf("Lookup acme/quote after its change: %+v", q)
	}
	if !slices.Equal(changed[len(changed)-2:], []string{"acme/book", "acme/quote"}) {
		t.Fatalf("OnChange saw %v", changed)
	}
	// A deleted tenant's services go.
	exec(`update kontra.tenants set deleted_at = now() where id = $1`, acme)
	s.Notify("acme/quote")
	if q, _ := s.Lookup(ctx, "acme", "quote"); q != nil {
		t.Fatal("a deleted tenant's service is still served")
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Lookup(ctx, "acme", "book"); b != nil {
		t.Fatal("a full re-read kept a deleted tenant's service")
	}
}

// fakeSeller serves an agent card under a base path, counting fetches; down makes every request fail.
type fakeSeller struct {
	mu      sync.Mutex
	card    []byte
	fetches int
	down    bool
	srv     *httptest.Server
}

func newFakeSeller(t *testing.T, cardJSON string) *fakeSeller {
	f := &fakeSeller{card: []byte(cardJSON)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down || r.URL.Path != "/base/.well-known/agent-card.json" {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		f.fetches++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(f.card)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

const fakeCard = `{"name":"hello","description":"says hello","version":"1.0.0",
"supportedInterfaces":[{"url":"http://elsewhere.invalid/base/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":{"streaming":false},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`

// The card lifecycle (spec §3; the audit's card gap): the keeper fetches relative to the base, the served
// card is persisted (upstream_card verbatim, card as json, card_fetched_at), every good fetch of the same
// bytes moves card_fetched_at, and another replica starting while the seller is down serves the stored card
// at once, with the same ETag. Dispatch gets the backend and the card's Info.
func TestCardKeepersPersistAndSeed(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	seller := newFakeSeller(t, fakeCard)
	cfg := parseConfig(t, `services:
  hello:
    kind: a2a
    backend: { a2a: { url: "`+seller.srv.URL+`/base", allow_private: true } }
    allow: ["org:default"]
`)
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080"})
	if err := s.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.Run(runCtx, time.Hour); close(done) }()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	missing := s.WaitCards(wctx)
	cancel()
	if len(missing) > 0 {
		t.Fatalf("no card for %v", missing)
	}
	hello, _ := s.Lookup(ctx, TeamTenant, "hello")
	served, err := hello.Cards()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(served.Body, []byte(`"url":"http://localhost:8080/agents/hello"`)) || !bytes.Equal(served.Upstream, []byte(fakeCard)) {
		t.Fatalf("served card %s", served.Body)
	}
	be, info, err := s.Backend(ctx, hello.Ref, &store.Attempt{Backend: store.BackendDirect})
	if err != nil || be.Origin().String() != seller.srv.URL || info.Path != "/base/rpc" || info.Binding != "JSONRPC" {
		t.Fatalf("Backend = %v %+v %v (the card never steers the dialer, D71)", be, info, err)
	}
	// A tunnel attempt of a service that is now direct never reaches the direct URL (spec §4).
	sid := uuid.Must(uuid.NewV7())
	if _, _, err := s.Backend(ctx, hello.Ref, &store.Attempt{Backend: store.BackendTunnel, TunnelSession: &sid}); err == nil {
		t.Fatal("a tunnel attempt was given the direct backend")
	}
	// Persisted by the replica that fetched it.
	var (
		up        []byte
		stored    string
		fetchedAt time.Time
	)
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := pool.QueryRow(ctx, `select upstream_card, card::text, card_fetched_at from kontra.services where name = 'hello'`).Scan(&up, &stored, &fetchedAt)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the card was not persisted: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !bytes.Equal(up, []byte(fakeCard)) || stored != string(served.Body) {
		t.Fatalf("persisted card %s / %s", up, stored)
	}
	// A later good fetch of the same bytes moves card_fetched_at.
	s.mu.RLock()
	k := s.byName["default/hello"].keeper
	s.mu.RUnlock()
	time.Sleep(10 * time.Millisecond)
	if err := k.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if moved := scalar[time.Time](t, pool, `select card_fetched_at from kontra.services where name = 'hello'`); !moved.After(fetchedAt) {
		t.Fatalf("card_fetched_at %v did not move after a good fetch (was %v)", moved, fetchedAt)
	}
	stop()
	<-done

	// Another replica starts while the seller is down: the stored card is served at once.
	seller.mu.Lock()
	seller.down = true
	seller.mu.Unlock()
	other := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080"})
	if err := other.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	o, _ := other.Lookup(ctx, TeamTenant, "hello")
	seeded, err := o.Cards()
	if err != nil || seeded.ETag != served.ETag || !bytes.Equal(seeded.Body, served.Body) {
		t.Fatalf("the seeded card: %v %+v (want ETag %s)", err, seeded, served.ETag)
	}
	if _, _, err := other.Backend(ctx, o.Ref, nil); err != nil {
		t.Fatalf("dispatch on the seeded card: %v", err)
	}
}

// keeperOf is the card keeper the registry runs for a service ("<tenant>/<name>").
func keeperOf(t *testing.T, s *Services, key string) *card.Keeper {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.byName[key]
	if e == nil || e.keeper == nil {
		t.Fatalf("no card keeper for %s", key)
	}
	return e.keeper
}

func helloConfig(t *testing.T, sellerURL, allow, extra string) *config.Config {
	t.Helper()
	return parseConfig(t, `services:
  hello:
    kind: a2a
    backend: { a2a: { url: "`+sellerURL+`/base", allow_private: true } }
    allow: [`+allow+`]
`+extra)
}

// A card that is not valid UTF-8 (here a description with a Latin-1 byte, which M1 served byte for byte and
// Postgres refuses as json) has no RFC 8785 form, so the gate can neither hash it for kontra.upstream and
// services.card_hash nor sign the served card (spec §3, D166). It is never installed, unsigned or altered: the
// service has no card (503 card routes, service_unavailable dispatch) and nothing is recorded. A valid card from
// the same seller afterwards is served and recorded as usual.
func TestUnsignableCardIsNotServed(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	raw := strings.Replace(fakeCard, `"says hello"`, "\"caf\xe9\"", 1)
	seller := newFakeSeller(t, raw)
	cfg := helloConfig(t, seller.srv.URL, `"org:default"`, "")
	keys, _ := jws.EphemeralKeySet()
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Signer: &card.JWSSigner{Keys: keys}})
	if err := s.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	k := keeperOf(t, s, "default/hello")
	if err := k.Refresh(ctx); !errors.Is(err, card.ErrBadCard) {
		t.Fatalf("a fetch of a card that is not UTF-8: %v, want ErrBadCard", err)
	}
	hello, _ := s.Lookup(ctx, TeamTenant, "hello")
	if served, err := hello.Cards(); !errors.Is(err, card.ErrUnavailable) || served != nil {
		t.Fatalf("an unsignable card is served: %v %+v", err, served)
	}
	if _, _, err := s.Backend(ctx, hello.Ref, nil); !errors.Is(err, card.ErrUnavailable) {
		t.Errorf("Backend for an unsignable card: %v", err)
	}
	var up []byte
	if err := pool.QueryRow(ctx, `select upstream_card from kontra.services where name = 'hello'`).Scan(&up); err != nil || up != nil {
		t.Fatalf("an unsignable card was recorded: %q %v", up, err)
	}
	seller.mu.Lock()
	seller.card = []byte(fakeCard)
	seller.mu.Unlock()
	if err := k.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if served, err := hello.Cards(); err != nil || !bytes.Contains(served.Body, []byte(`"signatures":[`)) {
		t.Fatalf("the valid card: %v", err)
	}
}

func TestCardPersistRetriedAfterAFailure(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	seller := newFakeSeller(t, fakeCard)
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080"})
	if err := s.Sync(ctx, helloConfig(t, seller.srv.URL, `"org:default"`, "")); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`create sequence public.fail_once`,
		`create function public.fail_once() returns trigger language plpgsql as $$
		 begin
		   if nextval('public.fail_once') = 1 then raise exception 'injected: the first card write fails'; end if;
		   return new;
		 end $$`,
		`create trigger fail_once before update of upstream_card on kontra.services
		   for each row when (new.upstream_card is distinct from old.upstream_card) execute function public.fail_once()`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	k := keeperOf(t, s, "default/hello")
	if err := k.Refresh(ctx); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("the first persist: %v", err)
	}
	if err := k.Refresh(ctx); err != nil {
		t.Fatalf("the second fetch of the same bytes: %v", err)
	}
	hello, _ := s.Lookup(ctx, TeamTenant, "hello")
	served, _ := hello.Cards()
	var (
		up     []byte
		stored *string
	)
	if err := pool.QueryRow(ctx, `select upstream_card, card::text from kontra.services where name = 'hello'`).Scan(&up, &stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(up, []byte(fakeCard)) || stored == nil || *stored != string(served.Body) {
		t.Fatalf("after a failed persist and a good fetch of the same bytes: upstream_card %q, card %v", up, stored)
	}
}

// services.card follows the served body when only the rewrite input changes: an allow: list of exactly
// [anonymous] drops the security schemes, and the new keeper, seeded from the same upstream bytes,
// records its body at its first good fetch.
func TestCardColumnFollowsTheServedBody(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	seller := newFakeSeller(t, fakeCard)
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080"})
	stored := func() string {
		t.Helper()
		return scalar[string](t, pool, `select coalesce(card::text, '') from kontra.services where name = 'hello'`)
	}
	if err := s.Sync(ctx, helloConfig(t, seller.srv.URL, `"org:default"`, "")); err != nil {
		t.Fatal(err)
	}
	if err := keeperOf(t, s, "default/hello").Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := stored(); !strings.Contains(got, `"securitySchemes"`) {
		t.Fatalf("services.card with allow [org:default]: %s", got)
	}
	if err := s.Sync(ctx, helloConfig(t, seller.srv.URL, "anonymous", "")); err != nil {
		t.Fatal(err)
	}
	if err := keeperOf(t, s, "default/hello").Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	hello, _ := s.Lookup(ctx, TeamTenant, "hello")
	served, err := hello.Cards()
	if err != nil || bytes.Contains(served.Body, []byte(`"securitySchemes"`)) {
		t.Fatalf("served with allow [anonymous]: %v %s", err, served.Body)
	}
	if got := stored(); got != string(served.Body) {
		t.Fatalf("services.card after allow: [anonymous]:\n%s\nserved:\n%s", got, served.Body)
	}
}

// A db-sourced row (Cloud, spec §13.5) gets the refusals the configuration has for the same service
// (config/validate.go's SPEC-CONFLICT resolution of D91): sealed on a direct backend, which no node unseals and the
// gate would relay in plaintext. Such a row is left out (the unknown-service answer, service_unavailable for its
// tasks) instead of served. Sealed tunnel rows are served since M3 WP-08, which lifts D144's refusal of them.
// seller_keys are verified since M3 WP-02, which
// lifts D90 and the seller_keys half of D144: rows with them are served on direct and tunnel services, unless
// the keys cannot be read (no key the gate verifies with), which leaves the row out rather than serve its card
// unverified.
func TestRegistryRefusesWhatTheConfigurationRefuses(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	acme := uuid.Must(uuid.NewV7())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'acme', 'account')`, acme)
	const direct = `{"a2a": {"url": "http://127.0.0.1:9/base", "allow_private": true}}`
	const mcpDirect = `{"mcp": {"url": "http://127.0.0.1:9/mcp", "allow_private": true}}`
	for _, row := range []struct {
		name, kind, backend, sealed string
		sellerKeys                  any
	}{
		{"plain", "a2a", direct, "off", nil},
		{"sealed-direct", "a2a", direct, "required", nil},
		{"optional-direct", "a2a", direct, "optional", nil},
		{"sealed-mcp", "mcp", mcpDirect, "required", nil},
		{"sealed-tunnel", "a2a", `{"tunnel": {}}`, "required", nil},
		{"optional-tunnel", "a2a", `{"tunnel": {}}`, "optional", nil},
		{"plain-tunnel", "a2a", `{"tunnel": {}}`, "off", nil},
		{"keys-direct", "a2a", direct, "off", sellerJWKS},
		{"keys-url-direct", "a2a", direct, "off", `{"url": "https://seller.example/jwks.json"}`},
		{"keys-empty", "a2a", direct, "off", `{"keys": []}`},
		{"keys-http-url", "a2a", direct, "off", `{"url": "http://seller.example/jwks.json"}`},
		{"keys-null", "a2a", direct, "off", `null`},
		{"keys-tunnel", "a2a", `{"tunnel": {}}`, "off", sellerJWKS},
		{"keys-mcp-tunnel", "mcp", `{"tunnel": {}}`, "off", `{"keys": []}`},
		{"keys-sealed-tunnel", "a2a", `{"tunnel": {}}`, "required", sellerJWKS},
	} {
		exec(`insert into kontra.services (tenant_id, name, kind, backend, allow, sealed, seller_keys, source)
			values ($1, $2, $3, $4::jsonb, '{org:acme}', $5, $6::jsonb, 'db')`, acme, row.name, row.kind, row.backend, row.sealed, row.sellerKeys)
	}
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Multi: true})
	if err := s.Sync(ctx, parseConfig(t, "tenancy: multi\n")); err != nil {
		t.Fatal(err)
	}
	for name, served := range map[string]bool{
		"plain": true, "sealed-direct": false, "optional-direct": false, "sealed-mcp": false,
		"sealed-tunnel": true, "optional-tunnel": true, "plain-tunnel": true,
		"keys-direct": true, "keys-url-direct": true, "keys-empty": false, "keys-http-url": false, "keys-null": true,
		"keys-tunnel": true, "keys-mcp-tunnel": true, "keys-sealed-tunnel": true,
	} {
		got, _ := s.Lookup(ctx, "acme", name)
		if (got != nil) != served {
			t.Errorf("acme/%s: served %v, want %v", name, got != nil, served)
		}
		if _, err := s.BackendFor(ctx, acme, name); (err == nil) != served {
			t.Errorf("acme/%s: BackendFor error %v, want served %v", name, err, served)
		}
	}
	// A row that becomes refusable is dropped on its NOTIFY, and served again once fixed.
	exec(`update kontra.services set sealed = 'required' where name = 'plain'`)
	s.Notify("acme/plain")
	if got, _ := s.Lookup(ctx, "acme", "plain"); got != nil {
		t.Fatal("a direct row set sealed is still served")
	}
	exec(`update kontra.services set sealed = 'off' where name = 'plain'`)
	s.Notify("acme/plain")
	if got, _ := s.Lookup(ctx, "acme", "plain"); got == nil {
		t.Fatal("the fixed row is not served")
	}

	// D166: a gate off loopback without a signing key leaves every a2a row out (their cards would be unsigned)
	// and serves the MCP ones.
	public := NewServices(pool, ServicesOptions{PublicURL: "https://gate.example", Multi: true})
	if err := public.Sync(ctx, parseConfig(t, "tenancy: multi\n")); err != nil {
		t.Fatal(err)
	}
	if got, _ := public.Lookup(ctx, "acme", "plain"); got != nil {
		t.Error("an a2a row is served unsigned off loopback")
	}
	if got, _ := public.Lookup(ctx, "acme", "keys-mcp-tunnel"); got == nil {
		t.Error("an MCP row is not served on a gate without a signing key")
	}
	keys, _ := jws.EphemeralKeySet()
	signed := NewServices(pool, ServicesOptions{PublicURL: "https://gate.example", Multi: true, Signer: &card.JWSSigner{Keys: keys}})
	if err := signed.Sync(ctx, parseConfig(t, "tenancy: multi\n")); err != nil {
		t.Fatal(err)
	}
	if got, _ := signed.Lookup(ctx, "acme", "plain"); got == nil {
		t.Error("an a2a row is not served by a gate with a signing key")
	}
}

// sellerJWKS is an inline seller key set (RFC 7515's example P-256 key).
const sellerJWKS = `{"keys": [{"kty": "EC", "crv": "P-256", "x": "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU", "y": "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}]}`

// The registry signs every served card and records its upstream card's hash (spec §3, D13, D166): the served
// card carries exactly the gate's signature, services.card stores it, and services.card_hash is the SHA-256 of
// the upstream card's JCS. With seller_keys a verified card is served and recorded; when the seller then
// publishes a card no key verifies, its card routes answer 503, a dispatch fails service_unavailable, and the
// row keeps the last verified card. A tunnel service whose card does not verify is not "pending" (retried):
// it fails service_unavailable too.
func TestCardSignedVerifiedAndHashed(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	gateKeys, _ := jws.EphemeralKeySet()
	seller, _ := jws.EphemeralKeySet()
	stranger, _ := jws.EphemeralKeySet()
	signedBy := func(k *jws.KeySet) string {
		sig, err := k.Sign([]byte(fakeCard), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		sb, _ := json.Marshal(sig)
		return strings.TrimSuffix(fakeCard, "}") + `,"signatures":[` + string(sb) + `]}`
	}
	good, bad := signedBy(seller), signedBy(stranger)
	fs := newFakeSeller(t, good)
	jwks, _ := seller.JWKS()
	cfg := helloConfig(t, fs.srv.URL, `"org:default"`, "    seller_keys: "+string(jwks)+"\n")
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Signer: &card.JWSSigner{Keys: gateKeys, JWKSURL: "http://localhost:8080/.well-known/jwks.json"}})
	if err := s.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	k := keeperOf(t, s, "default/hello")
	if err := k.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	hello, _ := s.Lookup(ctx, TeamTenant, "hello")
	served, err := hello.Cards()
	if err != nil || !served.Verified {
		t.Fatalf("a verified card: %+v %v", served, err)
	}
	var sigs []jws.Signature
	m, _ := strictjson.Members(served.Body)
	if err := json.Unmarshal(m["signatures"], &sigs); err != nil || len(sigs) != 1 {
		t.Fatalf("served signatures %s", m["signatures"])
	}
	pub, _ := jws.ParseJWKS(must(gateKeys.JWKS()))
	h, err := jws.Verify(served.Body, sigs[0], pub)
	if err != nil || h.Upstream == nil || !h.Upstream.Verified || h.Upstream.URL != "http://localhost:8080/agents/hello/.well-known/agent-card.upstream.json" {
		t.Fatalf("the gate's signature: %+v %v", h, err)
	}
	want, _ := card.UpstreamHash([]byte(good))
	var (
		hash   []byte
		stored string
	)
	if err := pool.QueryRow(ctx, `select card_hash, card::text from kontra.services where name = 'hello'`).Scan(&hash, &stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash, want[:]) || stored != string(served.Body) {
		t.Fatalf("card_hash %x (want %x), card %s", hash, want, stored)
	}
	// The seller publishes a card signed by another key.
	fs.mu.Lock()
	fs.card = []byte(bad)
	fs.mu.Unlock()
	if err := k.Refresh(ctx); !errors.Is(err, card.ErrNotVerified) {
		t.Fatalf("an unverifiable card: %v", err)
	}
	if c, err := hello.Cards(); err == nil || c == nil || !c.Unavailable {
		t.Fatalf("an unverifiable card is served: %+v %v", c, err)
	}
	if _, _, err := s.Backend(ctx, hello.Ref, nil); !errors.Is(err, card.ErrUnavailable) {
		t.Errorf("Backend for an unverifiable card: %v", err)
	}
	var up []byte
	if err := pool.QueryRow(ctx, `select upstream_card from kontra.services where name = 'hello'`).Scan(&up); err != nil || string(up) != good {
		t.Errorf("the row lost the last verified card: %s %v", up, err)
	}

	// A tunnel service whose card no key verifies fails service_unavailable, not the pending retry.
	tun := &fakeTunnels{}
	tun.set(bad, true)
	sid := uuid.Must(uuid.NewV7())
	sess := &fakeSessions{held: map[uuid.UUID]bool{sid: true}}
	ts := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: tun, Sessions: sess,
		Signer: &card.JWSSigner{Keys: gateKeys}})
	if err := ts.Sync(ctx, parseConfig(t, "services:\n  remote:\n    kind: a2a\n    backend: { tunnel: {} }\n    seller_keys: "+string(jwks)+"\n")); err != nil {
		t.Fatal(err)
	}
	tk := keeperOf(t, ts, "default/remote")
	if err := tk.Refresh(ctx); !errors.Is(err, card.ErrNotVerified) {
		t.Fatalf("a tunnel card no key verifies: %v", err)
	}
	remote, _ := ts.Lookup(ctx, TeamTenant, "remote")
	_, _, err = ts.Backend(ctx, remote.Ref, &store.Attempt{Backend: store.BackendTunnel, TunnelSession: &sid})
	if err == nil || errors.Is(err, relay.ErrCardPending) || !errors.Is(err, card.ErrNotVerified) {
		t.Errorf("Backend for a tunnel card no key verifies: %v", err)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// fakeTunnels is TunnelCards without sessions: it serves a card as if fetched through a session it holds
// while serving, and records the OnConnect callbacks.
type fakeTunnels struct {
	mu        sync.Mutex
	card      string
	serving   bool
	agentDown bool        // a session serves the service, but its node's agent is down: every fetch fails
	fetches   int         // fetches that returned the card
	tries     []time.Time // every fetch through a session, failed or not
	connect   []func(context.Context, uuid.UUID, string)
}

func (f *fakeTunnels) FetchCard(_ context.Context, _ uuid.UUID, _ string) (*upstream.Fetched, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.serving {
		return nil, errors.New("fakeTunnels: no session here")
	}
	f.tries = append(f.tries, time.Now())
	if f.agentDown {
		return nil, errors.New("fakeTunnels: 502 Kontra-Node: target_unreachable")
	}
	f.fetches++
	return &upstream.Fetched{Raw: []byte(f.card), Path: "/.well-known/agent-card.json"}, nil
}

func (f *fakeTunnels) setAgentDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentDown = down
}

// triesSince counts the fetches made at or after t.
func (f *fakeTunnels) triesSince(t time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, at := range f.tries {
		if !at.Before(t) {
			n++
		}
	}
	return n
}

func (f *fakeTunnels) OnConnect(fn func(context.Context, uuid.UUID, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connect = append(f.connect, fn)
}

func (f *fakeTunnels) Serves(uuid.UUID, string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.serving
}

func (f *fakeTunnels) set(card string, serving bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.card, f.serving = card, serving
}

func (f *fakeTunnels) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

// connected runs the OnConnect callbacks, as the registry does when a session for the service is added.
func (f *fakeTunnels) connected(tid uuid.UUID, service string) {
	f.mu.Lock()
	fns := slices.Clone(f.connect)
	f.mu.Unlock()
	for _, fn := range fns {
		fn(context.Background(), tid, service)
	}
}

const tunnelCardV1 = `{"name":"echo","description":"echoes","version":"1.0.0",
"supportedInterfaces":[{"url":"http://127.0.0.1:9999/","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":{"streaming":false},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`

// Tunnel cards (spec §3, M2): no card before a session connects (and WaitCards does not wait for one); when a
// session connects its card is fetched through it at once, stored (upstream_card verbatim, card as json,
// card_fetched_at) and served with the gate's interfaces; a replica without a session serves the stored card
// after the NOTIFY kontra_services the record sends, and fetches nothing; a refetch every TunnelCardRefresh
// while serving updates the card on both (logged, since a service's nodes should serve one card), and none
// runs without a session. The dispatch over the tunnel is WP-05's.
func TestTunnelCardKeepers(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	cfg := parseConfig(t, `services:
  echo:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
`)
	var logs logBuffer
	fa := &fakeTunnels{}
	a := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: fa, TunnelCardRefresh: 300 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	fb := &fakeTunnels{}
	b := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: fb, TunnelCardRefresh: 300 * time.Millisecond})
	for _, s := range []*Services{a, b} {
		if err := s.Sync(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { stop(); wg.Wait() }()
	for _, s := range []*Services{a, b} {
		wg.Go(func() { s.Run(runCtx, time.Hour) })
	}
	l := pg.NewListener(pool.Config().ConnConfig)
	l.OnChannel(pg.ChannelServices, b.Notify)
	wg.Go(func() { _ = l.Run(runCtx) })
	deadline := time.Now().Add(10 * time.Second)
	for !l.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("LISTEN not up")
		}
		time.Sleep(20 * time.Millisecond)
	}

	echo := func(s *Services) *a2ahttp.Service {
		t.Helper()
		e, _ := s.Lookup(ctx, TeamTenant, "echo")
		if e == nil {
			t.Fatal("echo is not served")
		}
		return e
	}
	if _, err := echo(a).Cards(); !errors.Is(err, card.ErrUnavailable) {
		t.Fatalf("a card before any session: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, time.Second)
	missing := a.WaitCards(wctx)
	cancel()
	if len(missing) > 0 {
		t.Fatalf("WaitCards waits for a tunnel card: %v", missing)
	}
	time.Sleep(time.Second) // no session: the refresh loop fetches nothing
	if fa.count() != 0 {
		t.Fatalf("%d fetches without a session", fa.count())
	}

	// A session connects on A: fetched at once, through it, before it is a candidate.
	fa.set(tunnelCardV1, true)
	tid := echo(a).Ref.TenantID
	fa.connected(tid, "echo")
	if fa.count() != 1 {
		t.Fatalf("%d fetches at the connect, want 1", fa.count())
	}
	served, err := echo(a).Cards()
	if err != nil || !bytes.Contains(served.Body, []byte(`"url":"http://localhost:8080/agents/echo"`)) ||
		bytes.Contains(served.Body, []byte("127.0.0.1:9999")) || string(served.Upstream) != tunnelCardV1 {
		t.Fatalf("A's card %s, %v", served.Body, err)
	}
	var (
		up        []byte
		stored    string
		fetchedAt time.Time
	)
	if err := pool.QueryRow(ctx, `select upstream_card, card::text, card_fetched_at from kontra.services where name = 'echo'`).Scan(&up, &stored, &fetchedAt); err != nil {
		t.Fatalf("the card was not stored: %v", err)
	}
	if string(up) != tunnelCardV1 || stored != string(served.Body) {
		t.Fatalf("stored %s / %s", up, stored)
	}
	// B has no session: it serves the stored card, the same body and ETag, and fetched nothing.
	waitCard := func(s *Services, want string) *card.Served {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			c, err := echo(s).Cards()
			if err == nil && string(c.Upstream) == want {
				return c
			}
			if time.Now().After(deadline) {
				t.Fatalf("the card %s is not served: %+v, %v", want, c, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if c := waitCard(b, tunnelCardV1); c.ETag != served.ETag || !bytes.Equal(c.Body, served.Body) {
		t.Fatalf("B serves %s (ETag %s), want A's (ETag %s)", c.Body, c.ETag, served.ETag)
	}
	if fb.count() != 0 {
		t.Fatalf("B fetched %d times without a session", fb.count())
	}

	// The refetch while serving: a new card reaches both.
	v2 := strings.Replace(tunnelCardV1, `"version":"1.0.0"`, `"version":"2.0.0"`, 1)
	fa.set(v2, true)
	waitCard(a, v2)
	waitCard(b, v2)
	if !strings.Contains(logs.String(), "differs from the one served") {
		t.Fatal("a changed card is not logged")
	}
	if stored := scalar[[]byte](t, pool, `select upstream_card from kontra.services where name = 'echo'`); string(stored) != v2 {
		t.Fatalf("stored %s after the refetch", stored)
	}
	// The session gone: no refetch.
	fa.set(v2, false)
	n := fa.count()
	time.Sleep(time.Second)
	if fa.count() != n {
		t.Fatalf("%d refetches without a session", fa.count()-n)
	}
	// A registry built without TunnelSessions dispatches no tunnel attempt.
	sid := uuid.Must(uuid.NewV7())
	if _, _, err := a.Backend(ctx, echo(a).Ref, &store.Attempt{Backend: store.BackendTunnel, TunnelSession: &sid}); err == nil ||
		errors.Is(err, upstream.ErrTunnelLost) || errors.Is(err, relay.ErrCardPending) {
		t.Fatalf("Backend of a tunnel service without sessions: %v", err)
	}
}

// fakeSessions is TunnelSessions: the sessions it holds, each answering a backend; any other is gone.
type fakeSessions struct {
	mu   sync.Mutex
	held map[uuid.UUID]bool
	asks []uuid.UUID
}

func (f *fakeSessions) Backend(id uuid.UUID, svc store.ServiceRef) (upstream.Backend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks = append(f.asks, id)
	if !f.held[id] {
		return nil, fmt.Errorf("%w: fakeSessions: no session %s", upstream.ErrTunnelLost, id)
	}
	return upstream.NewTunnel(svc.Name, "a2a", nopOpener{})
}

type nopOpener struct{}

func (nopOpener) OpenExchange(context.Context) (net.Conn, error) {
	return nil, errors.New("nopOpener: no stream")
}

// A tunnel attempt's backend (spec §6.2, §14.2): its session on this replica, with the Info of the
// service's one card. An attempt without a session, or whose session this replica does not hold, is a lost
// session (upstream.ErrTunnelLost) whatever the card, and is checked first; a held session of a service with
// no card yet is relay.ErrCardPending, which makes the card be fetched again at once (M2 plan audit, gap 3);
// with the card the backend is the session's tunnel and the Info the card's interface path.
func TestTunnelBackend(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	cfg := parseConfig(t, `services:
  echo:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
`)
	held, gone := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	ft := &fakeTunnels{}
	fs := &fakeSessions{held: map[uuid.UUID]bool{held: true}}
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: ft, Sessions: fs, TunnelCardRefresh: time.Hour})
	if err := s.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { stop(); wg.Wait() }()
	wg.Go(func() { s.Run(runCtx, time.Hour) })
	e, _ := s.Lookup(ctx, TeamTenant, "echo")
	ref := e.Ref
	on := func(id uuid.UUID) *store.Attempt {
		return &store.Attempt{Backend: store.BackendTunnel, TunnelSession: &id, KeyID: "k1_x", ReplicaID: "r1"}
	}
	for name, a := range map[string]*store.Attempt{
		"no attempt":              nil,
		"no session":              {Backend: store.BackendTunnel, Outcome: store.OutcomeNoSession},
		"a direct attempt":        {Backend: store.BackendDirect},
		"a session not held here": on(gone),
	} {
		if _, _, err := s.Backend(ctx, ref, a); !errors.Is(err, upstream.ErrTunnelLost) {
			t.Errorf("%s: %v, want a lost session", name, err)
		}
	}
	// The session is held, but no fetch has produced the card yet (the node's agent was not up): retried,
	// and the card is fetched again on the keeper's loop.
	if _, _, err := s.Backend(ctx, ref, on(held)); !errors.Is(err, relay.ErrCardPending) || errors.Is(err, upstream.ErrTunnelLost) {
		t.Fatalf("no card yet: %v", err)
	}
	ft.set(tunnelCardV1, true) // the agent is up now
	// Still pending, unless the keeper's loop, woken by the call above, has fetched the card already.
	if _, _, err := s.Backend(ctx, ref, on(held)); err != nil && !errors.Is(err, relay.ErrCardPending) {
		t.Fatalf("no card yet: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for ft.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a pending card was not fetched again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var (
		be   upstream.Backend
		info upstream.Info
		err  error
	)
	for {
		if be, info, err = s.Backend(ctx, ref, on(held)); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("with the card: %v", err)
	}
	tb, ok := be.(*upstream.Tunnel)
	if !ok || tb.Service != "echo" || be.Kind() != upstream.KindTunnel || info.Path != "/" || info.Binding != "JSONRPC" {
		t.Fatalf("backend %T %+v, info %+v", be, be, info)
	}
	// A direct attempt of the service (placed before it became a tunnel service) is not dispatched over a
	// session: a lost session, retried as a new attempt before the first upstream id.
	if _, _, err := s.Backend(ctx, ref, &store.Attempt{Backend: store.BackendDirect}); !errors.Is(err, upstream.ErrTunnelLost) {
		t.Fatalf("a direct attempt of a tunnel service: %v", err)
	}
	// The session is checked first: a gone session is a lost session even with the card.
	if _, _, err := s.Backend(ctx, ref, on(gone)); !errors.Is(err, upstream.ErrTunnelLost) {
		t.Fatalf("a gone session with the card: %v", err)
	}
	fs.mu.Lock()
	asks := slices.Clone(fs.asks)
	fs.mu.Unlock()
	if len(asks) == 0 || asks[len(asks)-1] != gone {
		t.Fatalf("sessions asked %v", asks)
	}
}

// A dispatch that finds no card (relay.ErrCardPending) has the card fetched again at once, however far the
// keeper's backoff has grown while the node's agent was down, and a burst of such dispatches neither
// postpones that fetch nor makes one fetch each (M2 review of WP-08: TestTunnelCardPending in test/e2e). A
// sidecar's agent that comes up after its node would otherwise see a task placed meanwhile spend its
// task.retries, 3 s with the default 2, waiting on a refetch due up to a minute later, and fail
// service_unavailable, since each dispatch's kick restarted that wait.
func TestTunnelCardPendingFetchesAtOnce(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	cfg := parseConfig(t, `services:
  echo:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
`)
	held := uuid.Must(uuid.NewV7())
	ft := &fakeTunnels{}
	ft.set(tunnelCardV1, true)
	ft.setAgentDown(true)
	fs := &fakeSessions{held: map[uuid.UUID]bool{held: true}}
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: ft, Sessions: fs, TunnelCardRefresh: time.Hour})
	if err := s.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { stop(); wg.Wait() }()
	wg.Go(func() { s.Run(runCtx, time.Hour) })
	e, _ := s.Lookup(ctx, TeamTenant, "echo")
	attempt := &store.Attempt{Backend: store.BackendTunnel, TunnelSession: &held, KeyID: "k1_x", ReplicaID: "r1"}

	// The agent is down: the keeper fetches at once, then 1 s and 2 s later; the next try is 4 s away.
	start := time.Now()
	eventually(t, 10*time.Second, "three failed fetches", func() bool { return ft.triesSince(start) >= 3 })
	if _, _, err := s.Backend(ctx, e.Ref, attempt); !errors.Is(err, relay.ErrCardPending) {
		t.Fatalf("no card yet: %v", err)
	}

	// The agent comes up; three dispatches, as a task's attempts would be, find no card.
	ft.setAgentDown(false)
	up := time.Now()
	for range 3 {
		if _, _, err := s.Backend(ctx, e.Ref, attempt); err == nil {
			break
		} else if !errors.Is(err, relay.ErrCardPending) {
			t.Fatalf("no card yet: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var err error
	deadline := up.Add(1500 * time.Millisecond)
	for {
		if _, _, err = s.Backend(ctx, e.Ref, attempt); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the card was not fetched within 1.5 s of the dispatches that found none (%d fetches since): %v", ft.triesSince(up), err)
	}
	if n := ft.triesSince(up); n > 2 {
		t.Fatalf("%d fetches for a burst of dispatches, want at most one a second", n)
	}
}

// A tunnel service whose stored card is past card.UnavailableAfter (its node was away for more than 30
// minutes) and whose node is back with its agent not up yet is in the same race as one that never had a
// card: a dispatch then is relay.ErrCardPending, retried, and has the card fetched again at once, not
// failed service_unavailable (M2 review, relay-tunnel-3; D143).
func TestTunnelExpiredCardIsPending(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	cfg := parseConfig(t, `services:
  echo:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
`)
	// Another replica fetched the card 31 minutes ago; the node then went away.
	first := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: &fakeTunnels{}, TunnelCardRefresh: time.Hour})
	if err := first.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update kontra.services set upstream_card = $1, card_fetched_at = now() - interval '31 minutes'
		where name = 'echo'`, []byte(tunnelCardV1)); err != nil {
		t.Fatal(err)
	}

	// The node is back on this replica, before its agent.
	held := uuid.Must(uuid.NewV7())
	ft := &fakeTunnels{}
	ft.set(tunnelCardV1, true)
	ft.setAgentDown(true)
	fs := &fakeSessions{held: map[uuid.UUID]bool{held: true}}
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: ft, Sessions: fs, TunnelCardRefresh: time.Hour})
	if err := s.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { stop(); wg.Wait() }()
	wg.Go(func() { s.Run(runCtx, time.Hour) })
	e, _ := s.Lookup(ctx, TeamTenant, "echo")
	if c, err := e.Cards(); !errors.Is(err, card.ErrUnavailable) || c == nil || !c.Unavailable || string(c.Upstream) != tunnelCardV1 {
		t.Fatalf("the stored card, 31 minutes old: %+v, %v; want it seeded and unavailable", c, err)
	}
	attempt := &store.Attempt{Backend: store.BackendTunnel, TunnelSession: &held, KeyID: "k1_x", ReplicaID: "r1"}
	eventually(t, 10*time.Second, "a failed fetch through the new session", func() bool { return ft.triesSince(time.Time{}) >= 1 })
	if _, _, err := s.Backend(ctx, e.Ref, attempt); !errors.Is(err, relay.ErrCardPending) || errors.Is(err, upstream.ErrTunnelLost) {
		t.Fatalf("a dispatch with the expired card and the agent down: %v, want relay.ErrCardPending", err)
	}

	// The agent comes up: the dispatch that finds no usable card has it fetched at once.
	ft.setAgentDown(false)
	up := time.Now()
	if _, _, err := s.Backend(ctx, e.Ref, attempt); !errors.Is(err, relay.ErrCardPending) {
		t.Fatalf("no usable card yet: %v", err)
	}
	var err error
	for deadline := up.Add(1500 * time.Millisecond); ; time.Sleep(20 * time.Millisecond) {
		if _, _, err = s.Backend(ctx, e.Ref, attempt); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("the card was not fetched within 1.5 s of the dispatch that found none usable: %v", err)
	}
}

// logBuffer is a logger's output, safe for concurrent writes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// The registry serves kind: mcp rows to the MCP relay (M3 WP-04): the services: block's tools, tasks and
// annotations (tool_meta, initialize_meta) as the relay reads them, a direct mcp: {url} backend pinned to its URL,
// none for a tunnel service; an a2a service is no MCP service; a change to an MCP row's policy is announced
// (OnChange, which drops the tools cache); a row whose tools cannot be read is left out.
func TestMCPServicesInTheRegistry(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	ctx := context.Background()
	var changed []string
	var mu sync.Mutex
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", OnChange: func(ref store.ServiceRef) {
		mu.Lock()
		changed = append(changed, ref.Name)
		mu.Unlock()
	}})
	cfg := parseConfig(t, `services:
  web:
    kind: mcp
    backend: { mcp: { url: "http://127.0.0.1:9/mcp", allow_private: true } }
    allow: ["org:default"]
    tools: { allow: ["read_*", "list_*"], deny: ["read_secret"] }
    tasks: passthrough
    session_idle_s: 120
    annotations: { tool_meta: { "*": { "sh.kontra/price": "1 USD" } }, initialize_meta: { "sh.kontra/price": "per session" } }
  fs:
    kind: mcp
    backend: { tunnel: {} }
    allow: ["org:default"]
    tools: { deny: ["write_*"] }
  hello:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
`)
	if err := s.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	web, err := s.MCPService(ctx, TeamTenant, "web")
	if err != nil || web == nil {
		t.Fatalf("web: %+v %v", web, err)
	}
	if web.Tasks != config.TasksPassthrough || web.SessionIdle != 120*time.Second || web.Direct == nil ||
		web.Direct.Base().String() != "http://127.0.0.1:9/mcp" || web.Ref.Kind != config.KindMCP {
		t.Errorf("web: %+v", web)
	}
	if ok, _ := web.Tools.Allows("read_file"); !ok {
		t.Error("read_file is not allowed")
	}
	if ok, _ := web.Tools.Allows("read_secret"); ok {
		t.Error("read_secret is allowed")
	}
	if ok, _ := web.Tools.Allows("write_file"); ok {
		t.Error("write_file is allowed outside allow")
	}
	if string(web.ToolMeta["*"]["sh.kontra/price"]) != `"1 USD"` || string(web.InitializeMeta["sh.kontra/price"]) != `"per session"` {
		t.Errorf("annotations %v %v", web.ToolMeta, web.InitializeMeta)
	}
	fs, _ := s.MCPService(ctx, TeamTenant, "fs")
	if fs == nil || fs.Direct != nil || fs.Tasks != config.TasksDurable {
		t.Errorf("fs: %+v", fs)
	}
	if ok, _ := fs.Tools.Allows("write_x"); ok {
		t.Error("deny-only policy allows write_x")
	}
	if ok, _ := fs.Tools.Allows("read_x"); !ok {
		t.Error("deny-only policy (no allow) refuses read_x")
	}
	if got := scalar[string](t, pool, `select tools::text from kontra.services where name = 'fs'`); strings.Contains(got, "allow") {
		t.Errorf("a configuration without allow stored one: %s", got)
	}
	if h, _ := s.MCPService(ctx, TeamTenant, "hello"); h != nil {
		t.Errorf("an a2a service is an MCP service: %+v", h)
	}
	if byID, _ := s.MCPServiceByID(ctx, web.Ref.TenantID, "web"); byID != web {
		t.Error("MCPServiceByID")
	}
	mu.Lock()
	changed = nil
	mu.Unlock()
	if _, err := pool.Exec(ctx, `update kontra.services set tools = '{"allow": [], "deny": []}' where name = 'fs'`); err != nil {
		t.Fatal(err)
	}
	s.Notify(TeamTenant + "/fs")
	mu.Lock()
	if !slices.Contains(changed, "fs") {
		t.Errorf("a policy change was not announced: %v", changed)
	}
	mu.Unlock()
	if fs, _ := s.MCPService(ctx, TeamTenant, "fs"); fs == nil {
		t.Fatal("fs is gone")
	} else if ok, _ := fs.Tools.Allows("read_x"); ok {
		t.Error("the new policy is not applied")
	}
	if _, err := pool.Exec(ctx, `update kontra.services set tools = '{"allow": "not a list"}' where name = 'fs'`); err != nil {
		t.Fatal(err)
	}
	s.Notify(TeamTenant + "/fs")
	if fs, _ := s.MCPService(ctx, TeamTenant, "fs"); fs != nil {
		t.Errorf("a row whose tools cannot be read is served: %+v", fs)
	}
	// Disabled: still found (the route answers it as unknown).
	if _, err := pool.Exec(ctx, `update kontra.services set disabled_at = now() where name = 'web'`); err != nil {
		t.Fatal(err)
	}
	s.Notify(TeamTenant + "/web")
	if web, _ := s.MCPService(ctx, TeamTenant, "web"); web == nil || !web.Disabled {
		t.Errorf("disabled web: %+v", web)
	}
}
