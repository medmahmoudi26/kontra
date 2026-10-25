package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/store"
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
	if _, _, err := s.Backend(ctx, hello.Ref); !errors.Is(err, card.ErrUnavailable) {
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
	be, info, err := s.Backend(ctx, hello.Ref)
	if err != nil || be.Origin().String() != seller.srv.URL || info.Path != "/base/rpc" || info.Binding != "JSONRPC" {
		t.Fatalf("Backend = %v %+v %v (the card never steers the dialer, D71)", be, info, err)
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
	if _, _, err := other.Backend(ctx, o.Ref); err != nil {
		t.Fatalf("dispatch on the seeded card: %v", err)
	}
}
