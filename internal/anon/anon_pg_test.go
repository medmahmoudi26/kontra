package anon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/pgtest"
)

// pgEnv is a migrated database with one replica row (anon_calls.replica_id references it).
type pgEnv struct {
	t       *testing.T
	dsn     string
	pool    *pgxpool.Pool
	replica string
}

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	dsn := pgtest.Migrated(t)
	e := &pgEnv{t: t, dsn: dsn, pool: pgtest.Pool(t, dsn), replica: "r-anon-test"}
	e.exec(`insert into kontra.gate_replicas (replica_id, version) values ($1, 'test')`, e.replica)
	return e
}

func (e *pgEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// key is a test node key: an id of the k1_ shape and a 32-byte public key.
func newKey(i int) Key {
	pub := make([]byte, 32)
	copy(pub, fmt.Sprintf("pub-%028d", i))
	id := "k1_" + strings.Repeat("a", 26)
	b := []byte(id)
	for j, n := len(b)-1, i; n > 0 && j > 3; j, n = j-1, n/26 {
		b[j] = byte('a' + n%26)
	}
	return Key{ID: string(b), PubKey: pub, NodeName: fmt.Sprintf("node-%d", i)}
}

// open runs Open in a transaction of its own, as the admission does, and commits it unless it failed.
func (e *pgEnv) open(lim Limits, req OpenRequest) (*Tenant, error) {
	e.t.Helper()
	var t *Tenant
	err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		var err error
		t, err = Open(context.Background(), tx, lim, req)
		return err
	})
	return t, err
}

func (e *pgEnv) mustOpen(lim Limits, req OpenRequest) *Tenant {
	e.t.Helper()
	t, err := e.open(lim, req)
	if err != nil {
		e.t.Fatalf("open %s: %v", req.Key.ID, err)
	}
	return t
}

func netOf(s string) netip.Prefix { return Network(netip.MustParseAddr(s)) }

func svcs(names ...string) []Service {
	out := make([]Service, len(names))
	for i, n := range names {
		kind := "a2a"
		if strings.HasPrefix(n, "m") {
			kind = "mcp"
		}
		out[i] = Service{Kind: kind, Name: n}
	}
	return out
}

// tenantOf is the key's tunnel_keys.tenant_id.
func (e *pgEnv) tenantOf(keyID string) uuid.UUID {
	return scalar[uuid.UUID](e.t, e.pool, `select tenant_id from kontra.tunnel_keys where key_id = $1`, keyID)
}

// T §6.2 (D277, D287): 3 live anonymous tenants per IPv4 /24 and per IPv6 /48, the 4th refused
// ErrTooManyTenants (rate_limited); another network is not counted; an expired tenant no longer counts; the network
// is stored as a CIDR, never the address.
func TestOpenCountsNetworks(t *testing.T) {
	e := newPGEnv(t)
	lim := Defaults
	i := 0
	opens := func(addrs ...string) []error {
		var errs []error
		for _, a := range addrs {
			i++
			_, err := e.open(lim, OpenRequest{Key: newKey(i), Network: netOf(a), Services: svcs("hello")})
			errs = append(errs, err)
		}
		return errs
	}
	for _, c := range []struct {
		name, a, b, c, d, other string
		net                     string
	}{
		{"IPv4 /24", "203.0.113.1", "203.0.113.2", "203.0.113.200", "203.0.113.99", "203.0.114.1", "203.0.113.0/24"},
		{"IPv6 /48", "2001:db8:1::1", "2001:db8:1:ffff::2", "2001:db8:1:1:1:1:1:1", "2001:db8:1::99", "2001:db8:2::1", "2001:db8:1::/48"},
	} {
		t.Run(c.name, func(t *testing.T) {
			errs := opens(c.a, c.b, c.c)
			if err := errors.Join(errs...); err != nil {
				t.Fatalf("the first three: %v", err)
			}
			if err := opens(c.d)[0]; !errors.Is(err, ErrTooManyTenants) {
				t.Fatalf("the 4th: %v, want ErrTooManyTenants", err)
			}
			if err := opens(c.other)[0]; err != nil {
				t.Fatalf("another network: %v", err)
			}
			if n := scalar[int](t, e.pool, `select count(*) from kontra.tenants where created_from = $1::cidr`, c.net); n != 3 {
				t.Errorf("%d tenants stored with created_from %s, want 3", n, c.net)
			}
			// One of them expires: the network has room again.
			e.exec(`update kontra.tenants set expires_at = now() - interval '1 second'
				where id = (select id from kontra.tenants where created_from = $1::cidr order by created_at limit 1)`, c.net)
			if err := opens(c.d)[0]; err != nil {
				t.Fatalf("after an expiry: %v", err)
			}
		})
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.tenants where kind = 'anonymous' and (masklen(created_from) not in (24, 48))`); n != 0 {
		t.Errorf("%d tenants stored a network that is not a /24 or /48", n)
	}
}

// T §6.2 (D287): the live anonymous tenants gate-wide are at most max_tenants; one more is ErrUnavailable
// (anonymous_unavailable), whatever its network.
func TestOpenMaxTenants(t *testing.T) {
	e := newPGEnv(t)
	lim := Defaults
	lim.MaxTenants = 2
	for i, a := range []string{"198.51.100.1", "2001:db8:9::1"} {
		e.mustOpen(lim, OpenRequest{Key: newKey(i + 1), Network: netOf(a)})
	}
	if _, err := e.open(lim, OpenRequest{Key: newKey(3), Network: netOf("192.0.2.1")}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("over max_tenants: %v", err)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.tunnel_keys`); n != 2 {
		t.Errorf("%d keys after the refusal, want 2", n)
	}
	// A resumed tenant is not a new one.
	if tn, err := e.open(lim, OpenRequest{Key: withTenant(newKey(1), e.tenantOf(newKey(1).ID)), Network: netOf("192.0.2.1")}); err != nil || !tn.Resumed {
		t.Fatalf("a resume at max_tenants: %+v %v", tn, err)
	}
}

func withTenant(k Key, t uuid.UUID) Key { k.TenantID = t; return k }

type svcRow struct {
	kind     string
	allow    []string
	disabled bool
	bound    bool
}

func (e *pgEnv) services(tenant uuid.UUID, key string) map[string]svcRow {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `select s.name, s.kind, s.allow, s.disabled_at is not null,
		exists (select 1 from kontra.key_services ks where ks.tenant_id = s.tenant_id and ks.service = s.name and ks.key_id = $2 and ks.revoked_at is null)
		from kontra.services s where s.tenant_id = $1`, tenant, key)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]svcRow{}
	for rows.Next() {
		var n string
		var r svcRow
		if err := rows.Scan(&n, &r.kind, &r.allow, &r.disabled, &r.bound); err != nil {
			e.t.Fatal(err)
		}
		out[n] = r
	}
	return out
}

func enabled(m map[string]svcRow) []string {
	var out []string
	for n, r := range m {
		if !r.disabled {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// T §6.1, §6.2 (D287): at most services services, counted per tenant: a hello declaring a 5th is
// ErrTooManyServices with nothing written; the services get allow [org:<slug>], plus anonymous when public, and the
// gate's anonymous card extension; the same key before expiry gets the same tenant (Resumed, the same end); a resumed
// hello disables and unbinds what it no longer declares, enables again what it declares, and applies public anew, so
// reconnecting with new names never takes the tenant past 4; a service redeclared with another kind is
// ErrKindMismatch.
func TestOpenServicesPerTenant(t *testing.T) {
	e := newPGEnv(t)
	lim := Defaults
	k := newKey(1)
	if _, err := e.open(lim, OpenRequest{Key: k, Network: netOf("203.0.113.5"), Services: svcs("a", "b", "c", "d", "e")}); !errors.Is(err, ErrTooManyServices) {
		t.Fatalf("5 services: %v", err)
	}
	for _, tbl := range []string{"tenants", "tunnel_keys", "services", "key_services"} {
		if n := scalar[int](t, e.pool, `select count(*) from kontra.`+tbl); n != 0 {
			t.Errorf("%d %s rows after the refusal", n, tbl)
		}
	}
	decl := svcs("a", "b", "c", "m")
	decl[0].Public = true
	tn := e.mustOpen(lim, OpenRequest{Key: k, Network: netOf("203.0.113.5"), Services: decl})
	if !IsSlug(tn.Slug) || tn.Resumed || !slices.Equal(tn.Enabled, []string{"a", "b", "c", "m"}) || len(tn.Disabled) != 0 {
		t.Fatalf("opened %+v", tn)
	}
	if d := time.Until(tn.ExpiresAt); d < lim.TTL-time.Minute || d > lim.TTL+time.Minute {
		t.Errorf("expires in %v, want about %v", d, lim.TTL)
	}
	got := e.services(tn.ID, k.ID)
	if !slices.Equal(got["a"].allow, []string{"org:" + tn.Slug, "anonymous"}) || !slices.Equal(got["b"].allow, []string{"org:" + tn.Slug}) ||
		got["m"].kind != "mcp" || !got["a"].bound || !got["m"].bound {
		t.Fatalf("services %+v", got)
	}
	var ann json.RawMessage
	if err := e.pool.QueryRow(context.Background(), `select annotations from kontra.services where tenant_id = $1 and name = 'a'`, tn.ID).Scan(&ann); err != nil {
		t.Fatal(err)
	}
	var a struct {
		CardExtensions []struct {
			URI      string         `json:"uri"`
			Required bool           `json:"required"`
			Params   map[string]any `json:"params"`
		} `json:"card_extensions"`
	}
	if err := json.Unmarshal(ann, &a); err != nil || len(a.CardExtensions) != 1 || a.CardExtensions[0].URI != ExtensionURI ||
		a.CardExtensions[0].Required || a.CardExtensions[0].Params["expiresAt"] != tn.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Fatalf("annotations %s", ann)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.services where tenant_id = $1 and listed`, tn.ID); n != 0 {
		t.Error("an anonymous service is listed")
	}

	// Resumed with new names, b no longer public-less: a stays (no longer public), e is new, b, c and m are disabled.
	k.TenantID = tn.ID
	decl2 := svcs("a", "e")
	decl2[1].Public = true
	tn2 := e.mustOpen(lim, OpenRequest{Key: k, Network: netOf("198.51.100.1"), Services: decl2})
	if tn2.ID != tn.ID || !tn2.Resumed || !tn2.ExpiresAt.Equal(tn.ExpiresAt) || !slices.Equal(tn2.Enabled, []string{"a", "e"}) ||
		!slices.Equal(tn2.Disabled, []string{"b", "c", "m"}) {
		t.Fatalf("resumed %+v", tn2)
	}
	got = e.services(tn.ID, k.ID)
	if en := enabled(got); !slices.Equal(en, []string{"a", "e"}) || got["b"].bound || got["c"].bound || !got["a"].bound ||
		!slices.Equal(got["a"].allow, []string{"org:" + tn.Slug}) || !slices.Equal(got["e"].allow, []string{"org:" + tn.Slug, "anonymous"}) {
		t.Fatalf("after the resume %+v", got)
	}
	// Reconnecting again and again with new names never takes the tenant past 4 services.
	for round := range 3 {
		names := []string{}
		for j := range 4 {
			names = append(names, fmt.Sprintf("r%d-%d", round, j))
		}
		e.mustOpen(lim, OpenRequest{Key: k, Network: netOf("198.51.100.1"), Services: svcs(names...)})
		if en := enabled(e.services(tn.ID, k.ID)); !slices.Equal(en, names) {
			t.Fatalf("round %d: enabled %v, want %v", round, en, names)
		}
	}
	// The old names come back: enabled and bound again.
	e.mustOpen(lim, OpenRequest{Key: k, Network: netOf("198.51.100.1"), Services: svcs("a", "b")})
	if got := e.services(tn.ID, k.ID); !slices.Equal(enabled(got), []string{"a", "b"}) || !got["b"].bound {
		t.Fatalf("enabled again %+v", got)
	}
	// m was mcp: declared as a2a, it is a kind mismatch, and nothing changes.
	if _, err := e.open(lim, OpenRequest{Key: k, Network: netOf("198.51.100.1"), Services: []Service{{Kind: "a2a", Name: "m"}}}); !errors.Is(err, ErrKindMismatch) {
		t.Fatalf("kind mismatch: %v", err)
	}
	if got := e.services(tn.ID, k.ID); !slices.Equal(enabled(got), []string{"a", "b"}) {
		t.Fatalf("after the refusal %v", enabled(got))
	}
	// Only one tenant was ever made.
	if n := scalar[int](t, e.pool, `select count(*) from kontra.tenants`); n != 1 {
		t.Errorf("%d tenants", n)
	}
}

// T §6.1 (D287): a slug that exists, a tombstone included, is drawn again.
func TestOpenDrawsAFreeSlug(t *testing.T) {
	e := newPGEnv(t)
	taken := "anon-00000000000000aa"
	e.exec(`insert into kontra.tenants (id, slug, kind, expires_at, created_from, deleted_at)
		values ($1, $2, 'anonymous', now() - interval '2 days', '192.0.2.0/24', now())`, uuid.Must(uuid.NewV7()), taken)
	draws := 0
	newSlug = func() (string, error) {
		draws++
		if draws <= 3 {
			return taken, nil
		}
		return NewSlug()
	}
	defer func() { newSlug = NewSlug }()
	tn := e.mustOpen(Defaults, OpenRequest{Key: newKey(1), Network: netOf("203.0.113.1")})
	if tn.Slug == taken || draws != 4 {
		t.Fatalf("slug %s after %d draws", tn.Slug, draws)
	}
	newSlug = func() (string, error) { return taken, nil }
	if _, err := e.open(Defaults, OpenRequest{Key: newKey(2), Network: netOf("203.0.113.2")}); err == nil || !strings.Contains(err.Error(), "no free slug") {
		t.Fatalf("every draw taken: %v", err)
	}
}

// listen subscribes to the gate's channels on a connection of its own and returns what arrived when stop is called.
func (e *pgEnv) listen() (stop func() []string) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, ch := range []string{"kontra_services", "kontra_revoke", "kontra_tunnel"} {
		if _, err := c.Exec(ctx, "listen "+ch); err != nil {
			e.t.Fatal(err)
		}
	}
	var (
		mu  sync.Mutex
		got []string
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			n, err := c.WaitForNotification(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, n.Channel+" "+n.Payload)
			mu.Unlock()
		}
	}()
	return func() []string {
		time.Sleep(300 * time.Millisecond)
		cancel()
		<-done
		c.Close(context.Background())
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// T §6.1, §12 (D277, D287): at expires_at the tenant's services are disabled and the key's bindings revoked, with
// NOTIFY kontra_services, kontra_revoke 'binding:…' and kontra_tunnel 'expired:<tenant>' (the holder closes the
// session); the key is not revoked, and its next Open opens a new tenant with a new slug, the key moved there with its
// old bindings deleted. Expire is idempotent.
func TestExpireThenANewTenant(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	k := newKey(1)
	tn := e.mustOpen(Defaults, OpenRequest{Key: k, Network: netOf("203.0.113.9"), Services: svcs("a", "m")})
	sw := NewSweeper(e.pool, Defaults, nil, nil)
	if n, err := sw.Expire(ctx); err != nil || n != 0 {
		t.Fatalf("a live tenant expired: %d %v", n, err)
	}
	e.exec(`update kontra.tenants set expires_at = now() - interval '1 second' where id = $1`, tn.ID)
	stop := e.listen()
	if n, err := sw.Expire(ctx); err != nil || n != 1 {
		t.Fatalf("Expire = %d %v", n, err)
	}
	got := stop()
	for _, want := range []string{"kontra_services " + tn.Slug + "/a", "kontra_services " + tn.Slug + "/m",
		"kontra_revoke binding:" + k.ID + "/a", "kontra_revoke binding:" + k.ID + "/m", "kontra_tunnel " + ExpiredPayload(tn.ID)} {
		if !slices.Contains(got, want) {
			t.Errorf("no NOTIFY %q in %q", want, got)
		}
	}
	if s := e.services(tn.ID, k.ID); len(enabled(s)) != 0 || s["a"].bound || s["m"].bound {
		t.Fatalf("after expiry %+v", s)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.tunnel_keys where key_id = $1 and revoked_at is null`, k.ID); n != 1 {
		t.Fatal("expiry revoked the key")
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.audit`); n != 1 {
		t.Errorf("%d audit rows, want the expiry's", n)
	}
	if n, err := sw.Expire(ctx); err != nil || n != 0 {
		t.Fatalf("Expire again = %d %v", n, err)
	}

	k.TenantID = tn.ID
	tn2 := e.mustOpen(Defaults, OpenRequest{Key: k, Network: netOf("203.0.113.9"), Services: svcs("a")})
	if tn2.ID == tn.ID || tn2.Slug == tn.Slug || tn2.Resumed {
		t.Fatalf("after expiry %+v, before %+v", tn2, tn)
	}
	if got := e.tenantOf(k.ID); got != tn2.ID {
		t.Fatalf("the key is in %s, want the new tenant %s", got, tn2.ID)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.key_services where key_id = $1 and tenant_id = $2`, k.ID, tn.ID); n != 0 {
		t.Errorf("%d bindings of the old tenant kept", n)
	}
	if s := e.services(tn2.ID, k.ID); !s["a"].bound {
		t.Fatalf("new tenant's services %+v", s)
	}
}

// fakeObjects records DeleteTask calls and fails while fail is set.
type fakeObjects struct {
	mu    sync.Mutex
	calls []uuid.UUID
	fail  bool
}

func (f *fakeObjects) DeleteTask(_ context.Context, _, task uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("bucket down")
	}
	f.calls = append(f.calls, task)
	return nil
}

// insertTask inserts a gate task of the tenant's service: state, principal; terminal states get terminal_at.
func (e *pgEnv) insertTask(tenant uuid.UUID, service, state, principal string) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	var term any
	if state == "completed" || state == "failed" || state == "canceled" || state == "rejected" {
		term = time.Now()
	}
	e.exec(`insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id, state, version,
		snapshot, status_at, terminal_at) values ($1, $2, $3, 'a2a', $4, $5, 'node', 'ctx', $6, 1, '{}', now(), $7)`,
		id, tenant, service, uuid.New(), principal, state, term)
	return id
}

// T §6.1 (D287): purge_after after expiry the purge deletes the keys still in the tenant (their bindings and
// tunnels), its MCP sessions (the gate's own /mcp ones, service NULL, included), calls, tasks (events and blobs),
// services, enrollment tokens and in-flight call rows, the bucket objects first; it keeps only the tenant row, a
// tombstone with deleted_at and created_from a CIDR. A bucket that fails keeps everything for the next pass.
func TestPurgeLeavesTheTombstone(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	lim := Defaults
	lim.PurgeAfter = time.Hour
	k := newKey(1)
	tn := e.mustOpen(lim, OpenRequest{Key: k, Network: netOf("203.0.113.9"), Services: svcs("a", "m")})
	task := e.insertTask(tn.ID, "a", "completed", "anonymous")
	e.exec(`insert into kontra.task_events (task_id, seq, kind, payload) values ($1, 1, 'task', '{"x":1}')`, task)
	e.exec(`insert into kontra.blobs (ref, tenant_id, task_id, size) values ($1, $2, $3, 10)`, strings.Repeat("a", 26), tn.ID, task)
	e.exec(`insert into kontra.calls (id, tenant_id, service, tool, buyer_id, principal, bytes_in, bytes_out, duration_ms)
		values ($1, $2, 'm', 'echo', $3, 'anonymous', 1, 1, 1)`, uuid.New(), tn.ID, uuid.New())
	e.exec(`insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal, via, protocol_version)
		values (sha256('s1'), $1, 'm', $2, 'anonymous', 'anonymous', '2025-11-25'),
		       (sha256('s2'), $1, null, $2, 'node:x', 'node', '2025-11-25')`, tn.ID, uuid.New())
	e.exec(`insert into kontra.tunnels (session_id, key_id, tenant_id, replica_id, services, cli_version)
		values ($1, $2, $3, $4, '{a}', 'kontra/test')`, uuid.New(), k.ID, tn.ID, e.replica)
	e.exec(`insert into kontra.enrollment_tokens (id, tenant_id, scope, bind_services, token_hash, expires_at)
		values ($1, $2, 'tenant', '{a}', sha256('tok'), now() + interval '1 minute')`, uuid.New(), tn.ID)
	e.exec(`insert into kontra.anon_calls (id, tenant_id, replica_id) values ($1, $2, $3)`, uuid.New(), tn.ID, e.replica)

	objects := &fakeObjects{fail: true}
	sw := NewSweeper(e.pool, lim, objects, nil)
	if n, err := sw.Purge(ctx); err != nil || n != 0 {
		t.Fatalf("a live tenant purged: %d %v", n, err)
	}
	// Expired, but not purge_after ago: kept.
	e.exec(`update kontra.tenants set expires_at = now() - interval '30 minutes' where id = $1`, tn.ID)
	if n, err := sw.Purge(ctx); err != nil || n != 0 {
		t.Fatalf("purged before purge_after: %d %v", n, err)
	}
	e.exec(`update kontra.tenants set expires_at = now() - interval '2 hours' where id = $1`, tn.ID)
	if n, err := sw.Purge(ctx); err == nil || n != 0 {
		t.Fatalf("the bucket failed, yet %d purged (%v)", n, err)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.tasks where tenant_id = $1`, tn.ID); n != 1 {
		t.Fatal("the rows went before the objects")
	}
	objects.fail = false
	if n, err := sw.Purge(ctx); err != nil || n != 1 {
		t.Fatalf("Purge = %d %v", n, err)
	}
	if !slices.Equal(objects.calls, []uuid.UUID{task}) {
		t.Errorf("objects deleted for %v, want %v", objects.calls, task)
	}
	for _, tbl := range []string{"tunnel_keys", "key_services", "tunnels", "services", "tasks", "task_events", "blobs", "calls",
		"mcp_sessions", "enrollment_tokens", "anon_calls"} {
		if n := scalar[int](t, e.pool, `select count(*) from kontra.`+tbl); n != 0 {
			t.Errorf("%d %s rows after the purge", n, tbl)
		}
	}
	var (
		deleted *time.Time
		from    netip.Prefix
	)
	if err := e.pool.QueryRow(ctx, `select deleted_at, created_from from kontra.tenants where id = $1`, tn.ID).Scan(&deleted, &from); err != nil {
		t.Fatal(err)
	}
	if deleted == nil || from.String() != "203.0.113.0/24" {
		t.Fatalf("tombstone: deleted_at %v, created_from %s", deleted, from)
	}
	if n, err := sw.Purge(ctx); err != nil || n != 0 {
		t.Fatalf("purged twice: %d %v", n, err)
	}
	// The slug stays taken: a new tenant never gets it (TestOpenDrawsAFreeSlug draws again past a tombstone).
	if n := scalar[int](t, e.pool, `select count(*) from kontra.tenants where slug = $1`, tn.Slug); n != 1 {
		t.Fatal("the tombstone is gone")
	}
}

// contend runs n calls of fn at once while a separate connection holds lockSQL's row lock, and releases it only once
// all n wait on a lock, so they really race for it.
func (e *pgEnv) contend(n int, lockSQL string, lockArgs []any, fn func(i int) error) []error {
	e.t.Helper()
	ctx := context.Background()
	// Open the pool's connections first, so the calls do not queue behind connection setup on a loaded host.
	conns := make([]*pgxpool.Conn, 0, n)
	for range n {
		c, err := e.pool.Acquire(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	locker, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		e.t.Fatal(err)
	}
	defer locker.Close(context.Background())
	tx, err := locker.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, lockSQL, lockArgs...); err != nil {
		e.t.Fatal(err)
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = fn(i) })
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		var waiting int
		if err := locker.QueryRow(ctx, `select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			e.t.Fatal(err)
		}
		if waiting == n {
			break
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%d of %d waiting", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	wg.Wait()
	return errs
}

// T §6.2 (D277): concurrent tasks are at most tasks as host (the tenant's services' non-terminal tasks, whoever
// called) and as hirer (the non-terminal tasks whose principal is a key of the tenant): the 5th is ErrTooManyTasks
// (429), a terminal task does not count, a team tenant is not counted, and two creators at 3 never both pass.
func TestCapsTask(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	k := newKey(1)
	tn := e.mustOpen(Defaults, OpenRequest{Key: k, Network: netOf("203.0.113.9"), Services: svcs("a")})
	team := uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'default', 'team')`, team)
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, 'seller', 'a2a', '{"tunnel": {}}', 'config')`, team)
	caps := NewCaps(e.pool, Defaults, e.replica, nil)
	check := func(tenant uuid.UUID, slug, principal string) error {
		return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return caps.Task(ctx, tx, tenant, slug, principal) })
	}
	node := "node:" + k.ID

	// As host.
	e.insertTask(tn.ID, "a", "completed", "anonymous")
	for range 4 {
		if err := check(tn.ID, tn.Slug, "anonymous"); err != nil {
			t.Fatal(err)
		}
		e.insertTask(tn.ID, "a", "working", "anonymous")
	}
	if err := check(tn.ID, tn.Slug, "anonymous"); !errors.Is(err, ErrTooManyTasks) || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the 5th as host: %v", err)
	}
	// A team service is not counted, nor a team key.
	for range 6 {
		e.insertTask(team, "seller", "working", "key:kk_team_x")
	}
	if err := check(team, "default", "key:kk_team_x"); err != nil {
		t.Fatalf("a team tenant: %v", err)
	}
	// As hirer: the anonymous key's tasks at a team service.
	for range 4 {
		if err := check(team, "default", node); err != nil {
			t.Fatal(err)
		}
		e.insertTask(team, "seller", "input-required", node)
	}
	if err := check(team, "default", node); !errors.Is(err, ErrTooManyTasks) {
		t.Fatalf("the 5th as hirer: %v", err)
	}
	// Room again as host once tasks end: two concurrent creators at 3 never both pass.
	e.exec(`update kontra.tasks set state = 'completed', terminal_at = now() where tenant_id = $1 and state = 'working'
		and id = (select id from kontra.tasks where tenant_id = $1 and state = 'working' order by id limit 1)`, tn.ID)
	errs := e.contend(2, `select 1 from kontra.tenants where id = $1 for no key update`, []any{tn.ID}, func(int) error {
		return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			if err := caps.Task(ctx, tx, tn.ID, tn.Slug, "anonymous"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `insert into kontra.tasks (id, tenant_id, service, protocol, buyer_id, principal, via, context_id, state,
				version, snapshot, status_at) values ($1, $2, 'a', 'a2a', $3, 'anonymous', 'anonymous', 'ctx', 'working', 1, '{}', now())`,
				uuid.Must(uuid.NewV7()), tn.ID, uuid.New())
			return err
		})
	})
	passed, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			passed++
		case errors.Is(err, ErrTooManyTasks):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if passed != 1 || refused != 1 {
		t.Fatalf("two creators at 3: %d passed, %d refused (%v)", passed, refused, errs)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.tasks where tenant_id = $1 and terminal_at is null`, tn.ID); n != 4 {
		t.Fatalf("%d open tasks, want 4", n)
	}
}

// T §6.2 (D287): plain MCP calls in flight to an anonymous tenant's services are at most calls, gate-wide in
// anon_calls: the next is ErrTooManyCalls (429), a release frees a slot, and a replica that dies takes its slots with
// it; a team tenant's service takes none.
func TestCapsCall(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	tn := e.mustOpen(Defaults, OpenRequest{Key: newKey(1), Network: netOf("203.0.113.9"), Services: svcs("m")})
	lim := Defaults
	lim.Calls = 2
	other := "r-other"
	e.exec(`insert into kontra.gate_replicas (replica_id, version) values ($1, 'test')`, other)
	here, there := NewCaps(e.pool, lim, e.replica, nil), NewCaps(e.pool, lim, other, nil)
	r1, err := here.Call(ctx, tn.ID, tn.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := there.Call(ctx, tn.ID, tn.Slug); err != nil {
		t.Fatal(err)
	}
	if _, err := here.Call(ctx, tn.ID, tn.Slug); !errors.Is(err, ErrTooManyCalls) || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the 3rd call (the 2nd on another replica): %v", err)
	}
	r1()
	r1() // once
	r3, err := here.Call(ctx, tn.ID, tn.Slug)
	if err != nil {
		t.Fatalf("after a release: %v", err)
	}
	defer r3()
	e.exec(`delete from kontra.gate_replicas where replica_id = $1`, other) // reaped (T §11)
	if _, err := here.Call(ctx, tn.ID, tn.Slug); err != nil {
		t.Fatalf("after the other replica died: %v", err)
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.anon_calls`); n != 2 {
		t.Errorf("%d rows in flight, want 2", n)
	}
	release, err := here.Call(ctx, uuid.New(), "default")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if n := scalar[int](t, e.pool, `select count(*) from kontra.anon_calls`); n != 2 {
		t.Errorf("a team service took a slot")
	}
}

// T §6.2, relay §6.6 (D287): a caller's message that would take the tenant's stored bytes (its tasks' events and
// objects) past parts_bytes is ErrStorageFull; a dispatch counts the tenant's other tasks and its limit is
// min(task_bytes + others, parts_bytes); a team service is never counted.
func TestCapsStorage(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	tn := e.mustOpen(Defaults, OpenRequest{Key: newKey(1), Network: netOf("203.0.113.9"), Services: svcs("a")})
	lim := Defaults
	lim.PartsBytes = 1000
	caps := NewCaps(e.pool, lim, e.replica, nil)
	t1, t2 := e.insertTask(tn.ID, "a", "completed", "anonymous"), e.insertTask(tn.ID, "a", "working", "anonymous")
	payload := `{"text":"` + strings.Repeat("x", 290) + `"}` // 301 bytes
	e.exec(`insert into kontra.task_events (task_id, seq, kind, payload) values ($1, 1, 'task', $2::json), ($3, 1, 'task', $2::json)`, t1, payload, t2)
	e.exec(`insert into kontra.blobs (ref, tenant_id, task_id, size) values ($1, $2, $3, 100)`, strings.Repeat("b", 26), tn.ID, t1)
	if n, err := caps.Stored(ctx, tn.ID, uuid.Nil); err != nil || n != 702 {
		t.Fatalf("stored %d %v, want 702", n, err)
	}
	if err := caps.Message(ctx, tn.ID, tn.Slug, 298); err != nil {
		t.Fatalf("a message that fits: %v", err)
	}
	if err := caps.Message(ctx, tn.ID, tn.Slug, 299); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("a message one byte over: %v", err)
	}
	if err := caps.Message(ctx, uuid.New(), "default", 1<<40); err != nil {
		t.Fatalf("a team service: %v", err)
	}
	others, limit, applies, err := caps.Budget(ctx, tn.ID, tn.Slug, t2, 256<<20)
	if err != nil || !applies || others != 401 || limit != 1000 {
		t.Fatalf("budget of t2: others %d, limit %d, %v %v", others, limit, applies, err)
	}
	// The slug read when the dispatch does not hold it (an MCP task).
	if o2, l2, a2, err := caps.Budget(ctx, tn.ID, "", t2, 256<<20); err != nil || o2 != others || l2 != limit || !a2 {
		t.Fatalf("budget without the slug: %d %d %v %v", o2, l2, a2, err)
	}
	// task_bytes below the room left: the task's own limit stays task_bytes.
	if _, limit, _, _ := caps.Budget(ctx, tn.ID, tn.Slug, t2, 100); limit != 501 {
		t.Fatalf("limit with task_bytes 100: %d, want 501", limit)
	}
	if _, limit, applies, err := caps.Budget(ctx, uuid.New(), "default", t2, 7); err != nil || applies || limit != 7 {
		t.Fatalf("a team service's budget: %d %v %v", limit, applies, err)
	}
}

// MoveKey deletes the key's bindings before it moves the key (key_services' foreign key has no ON UPDATE), and
// refuses a key that does not exist.
func TestMoveKey(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	k := newKey(1)
	tn := e.mustOpen(Defaults, OpenRequest{Key: k, Network: netOf("203.0.113.9"), Services: svcs("a", "b")})
	to := uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, 'acme', 'account')`, to)
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return MoveKey(ctx, tx, k.ID, to) }); err != nil {
		t.Fatal(err)
	}
	if e.tenantOf(k.ID) != to || scalar[int](t, e.pool, `select count(*) from kontra.key_services where key_id = $1`, k.ID) != 0 {
		t.Fatal("not moved")
	}
	if n := scalar[int](t, e.pool, `select count(*) from kontra.services where tenant_id = $1`, tn.ID); n != 2 {
		t.Error("the move deleted services")
	}
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return MoveKey(ctx, tx, "k1_"+strings.Repeat("z", 26), to) }); err == nil {
		t.Error("moved a key that does not exist")
	}
}
