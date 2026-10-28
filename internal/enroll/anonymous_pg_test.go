package enroll

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/tunnel"
)

// newAnonEnv is a multi-tenant gate's admission with tunnel.anonymous (lim; §6.2's defaults when nil).
func newAnonEnv(t *testing.T, lim *anon.Limits) *pgEnv {
	t.Helper()
	if lim == nil {
		l := anon.Defaults
		lim = &l
	}
	return newPGEnv(t, func(o *Options) { o.Multi = true; o.Anonymous = lim })
}

// admitFrom admits h from the client address addr.
func (e *pgEnv) admitFrom(k *testKey, h *tunnel.Hello, addr string) (*Decision, error) {
	req := Request{Hello: h, PubKey: k.pub, SessionID: uuid.Must(uuid.NewV7()), ReplicaID: e.replic,
		Client: netip.MustParseAddr(addr)}
	return e.store.Admit(context.Background(), req)
}

func anonHello(k *testKey, services ...tunnel.Service) *tunnel.Hello {
	h := hello(k, services...)
	h.Anonymous = true
	return h
}

// spec/tunnel.md §6, §6.1 (DM4-18): a hello asking for an anonymous session (anonymous: true, neither enroll nor auth)
// from an unknown key opens a tenant anon-<16 hex> from the client's /24: the declared services at the multi-tenant
// URLs, welcome.anonymous.expires_at, the hiring principal node:<key_id>, allow [org:<slug>] plus anonymous for the
// public names, and an audit row; the client's address is never stored.
func TestAdmitAnonymousOpens(t *testing.T) {
	e := newAnonEnv(t, nil)
	ctx := context.Background()
	k := newTestKey(t)
	h := anonHello(k, a2a("hello"), mcp("files"))
	h.Public = []string{"hello", "hello"}
	before := time.Now()
	d, err := e.admitFrom(k, h, "203.0.113.77")
	if err != nil {
		t.Fatal(err)
	}
	slug := d.Key.Tenant
	if !anon.IsSlug(slug) || d.Key.KeyID != k.kid || d.HirePrincipal != "node:"+k.kid || d.Hire != nil || d.Anonymous == nil ||
		d.Anonymous.Resumed || !slices.Equal(d.Anonymous.Public, []string{"hello"}) {
		t.Fatalf("decision %+v %+v", d, d.Anonymous)
	}
	if got := served(d); !slices.Equal(got, []string{"a2a:hello@https://gate.example/agents/" + slug + "/hello",
		"mcp:files@https://gate.example/mcp/" + slug + "/files"}) {
		t.Errorf("served %v", got)
	}
	if exp := d.Anonymous.ExpiresAt; exp.Before(before.Add(2*time.Hour-time.Minute)) || exp.After(time.Now().Add(2*time.Hour+time.Minute)) {
		t.Errorf("expires at %s, want 2 h from now", exp)
	}
	var (
		kind string
		from netip.Prefix
	)
	if err := e.pool.QueryRow(ctx, `select kind, created_from from kontra.tenants where id = $1`, d.Key.TenantID).Scan(&kind, &from); err != nil ||
		kind != "anonymous" || from.String() != "203.0.113.0/24" {
		t.Fatalf("tenant %s %s %v", kind, from, err)
	}
	var allow []string
	if err := e.pool.QueryRow(ctx, `select allow from kontra.services where tenant_id = $1 and name = 'hello'`, d.Key.TenantID).Scan(&allow); err != nil ||
		!slices.Equal(allow, []string{"org:" + slug, "anonymous"}) {
		t.Fatalf("hello's allow %v %v", allow, err)
	}
	if err := e.pool.QueryRow(ctx, `select allow from kontra.services where tenant_id = $1 and name = 'files'`, d.Key.TenantID).Scan(&allow); err != nil ||
		!slices.Equal(allow, []string{"org:" + slug}) {
		t.Fatalf("files' allow %v %v", allow, err)
	}
	ev := e.verifyAudit()
	if len(ev) != 1 || ev[0]["event"] != EventAnonymous || ev[0]["tenant"] != slug || ev[0]["resumed"] != false {
		t.Fatalf("audit %v", ev)
	}
	if n := e.count(`select count(*) from kontra.audit where position('203.0.113.77'::bytea in row_jcs) > 0`); n != 0 {
		t.Error("the audit row holds the client's address")
	}
}

// DM4-18, §6, §6.2: without anonymous: true an unknown key stays unknown_key on a gate with anonymous sessions (the
// lost-welcome probe relies on it), and so does a probe from a key holding an anonymous session; a gate without
// tunnel.anonymous answers the field from an unknown key anonymous_unavailable; the limits' refusals are the
// handshake's codes (rate_limited, anonymous_unavailable, service_denied with nothing enrolled, kind_mismatch).
func TestAdmitAnonymousRefusals(t *testing.T) {
	t.Run("gate without tunnel.anonymous", func(t *testing.T) {
		e := newPGEnv(t, func(o *Options) { o.Multi = true })
		k := newTestKey(t)
		_, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113.1")
		wantCode(t, "anonymous: true on a gate without it", err, tunnel.CodeAnonymousUnavailable)
		_, err = e.admitFrom(k, hello(k, a2a("hello")), "203.0.113.1")
		wantCode(t, "no field", err, tunnel.CodeUnknownKey)
		if n := e.count(`select count(*) from kontra.tenants`); n != 1 { // the team tenant
			t.Errorf("%d tenants", n)
		}
	})
	t.Run("limits", func(t *testing.T) {
		lim := anon.Defaults
		lim.MaxTenants = 4
		e := newAnonEnv(t, &lim)
		k := newTestKey(t)
		_, err := e.admitFrom(k, hello(k, a2a("hello")), "203.0.113.1")
		wantCode(t, "an unknown key without the field (a probe)", err, tunnel.CodeUnknownKey)
		h := anonHello(k, a2a("a"), a2a("b"), a2a("c"), a2a("d"), a2a("e"))
		_, err = e.admitFrom(k, h, "203.0.113.1")
		wantCode(t, "a 5th declared service", err, tunnel.CodeServiceDenied)
		h = anonHello(k, a2a("a"))
		h.Public = []string{"b"}
		_, err = e.admitFrom(k, h, "203.0.113.1")
		wantCode(t, "a public name not declared", err, tunnel.CodeServiceDenied)
		for _, tbl := range []string{"tunnel_keys", "key_services", "services"} {
			if n := e.count(`select count(*) from kontra.` + tbl); n != 0 {
				t.Errorf("%d %s rows after the refusals", n, tbl)
			}
		}
		if n := e.count(`select count(*) from kontra.tenants where kind = 'anonymous'`); n != 0 {
			t.Errorf("%d anonymous tenants after the refusals", n)
		}
		for i := range 3 {
			k := newTestKey(t)
			if _, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113."+string(rune('1'+i))); err != nil {
				t.Fatal(err)
			}
		}
		k4 := newTestKey(t)
		_, err = e.admitFrom(k4, anonHello(k4, a2a("hello")), "203.0.113.250")
		wantCode(t, "a 4th from one /24", err, tunnel.CodeRateLimited)
		k5 := newTestKey(t)
		if _, err := e.admitFrom(k5, anonHello(k5, a2a("hello")), "2001:db8:5::1"); err != nil {
			t.Fatal(err)
		}
		k6 := newTestKey(t)
		_, err = e.admitFrom(k6, anonHello(k6, a2a("hello")), "2001:db8:6::1")
		wantCode(t, "one past max_tenants", err, tunnel.CodeAnonymousUnavailable)
		// A resumed session redeclaring a service with the other kind.
		_, err = e.admitFrom(k5, anonHello(k5, mcp("hello")), "2001:db8:5::1")
		wantCode(t, "another kind", err, tunnel.CodeKindMismatch)
	})
}

// §6.1 (DM4-18, DM4-19): the same key before expiry gets its tenant back (with the field, or with a hire-only auth key
// as its hiring principal, nothing converted); a probe from it (no field, no tokens, no auth) is unknown_key; at the
// end its bindings are revoked, not the key, and its next hello asking for a session opens a new tenant with a new
// slug, while one that does not ask is unknown_key.
func TestAdmitAnonymousResumeAndExpiry(t *testing.T) {
	e := newAnonEnv(t, nil)
	ctx := context.Background()
	k := newTestKey(t)
	d1, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := e.admitFrom(k, anonHello(k, a2a("hello"), a2a("more")), "198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Key.TenantID != d1.Key.TenantID || !d2.Anonymous.Resumed || !d2.Anonymous.ExpiresAt.Equal(d1.Anonymous.ExpiresAt) || len(d2.Served) != 2 {
		t.Fatalf("resumed %+v %+v, first %+v", d2.Key, d2.Anonymous, d1.Key)
	}
	_, err = e.admitFrom(k, hello(k, a2a("hello")), "203.0.113.7")
	wantCode(t, "a probe from the anonymous key", err, tunnel.CodeUnknownKey)

	hireOnly, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: e.team, Name: "hirer", Scopes: []auth.Scope{auth.ScopeHire}})
	if err != nil {
		t.Fatal(err)
	}
	h := hello(k, a2a("hello"))
	h.Auth = hireOnly.Secret
	d3, err := e.admitFrom(k, h, "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	if d3.Key.TenantID != d1.Key.TenantID || d3.Anonymous == nil || d3.Hire == nil || d3.HirePrincipal != d3.Hire.ID ||
		!strings.HasPrefix(d3.HirePrincipal, "key:") {
		t.Fatalf("hire-only auth: %+v %+v", d3.Key, d3.Hire)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2`, k.kid, d1.Key.TenantID); n != 1 {
		t.Fatal("a hire-only auth moved the key")
	}

	e.exec(`update kontra.tenants set expires_at = now() - interval '1 second' where id = $1`, d1.Key.TenantID)
	if n, err := anon.NewSweeper(e.pool, anon.Defaults, nil, nil).Expire(ctx); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and revoked_at is null`, k.kid); n != 1 {
		t.Fatal("expiry revoked the key")
	}
	_, err = e.admitFrom(k, hello(k, a2a("hello")), "203.0.113.7")
	wantCode(t, "a hello that does not ask, after expiry", err, tunnel.CodeUnknownKey)
	d4, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	if d4.Key.TenantID == d1.Key.TenantID || d4.Key.Tenant == d1.Key.Tenant || !anon.IsSlug(d4.Key.Tenant) || d4.Anonymous.Resumed {
		t.Fatalf("after expiry %+v, before %+v", d4.Key, d1.Key)
	}
	if n := e.count(`select count(*) from kontra.key_services where key_id = $1 and tenant_id = $2`, k.kid, d1.Key.TenantID); n != 0 {
		t.Errorf("%d bindings left in the expired tenant", n)
	}
}

// DM4-19: enroll tokens from a key of a live anonymous tenant convert it into the tokens' tenant, bound as §5 says
// (the key moved, its anonymous bindings deleted first); the anonymous tenant ends at once (expires_at now, services
// disabled, bindings revoked), and the enrollment's audit row names where the key came from.
func TestAdmitEnrollFromAnAnonymousKeyConverts(t *testing.T) {
	e := newAnonEnv(t, nil)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	k := newTestKey(t)
	d1, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	h := hello(k, a2a("hello"))
	h.Enroll = []string{e.token(e.team, "hello")}
	d2, err := e.admitFrom(k, h, "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Anonymous != nil || d2.Key.TenantID != e.team || d2.Key.Tenant != "default" ||
		!slices.Equal(served(d2), []string{"a2a:hello@https://gate.example/agents/default/hello"}) {
		t.Fatalf("converted %+v %v", d2.Key, served(d2))
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2 and revoked_at is null`, k.kid, e.team); n != 1 {
		t.Fatal("the key did not move")
	}
	var ended bool
	if err := e.pool.QueryRow(ctx, `select expires_at <= now() from kontra.tenants where id = $1`, d1.Key.TenantID).Scan(&ended); err != nil || !ended {
		t.Fatalf("the anonymous tenant did not end: %v", err)
	}
	if n := e.count(`select count(*) from kontra.services where tenant_id = $1 and disabled_at is null`, d1.Key.TenantID); n != 0 {
		t.Errorf("%d anonymous services still enabled", n)
	}
	var moved bool
	for _, r := range e.verifyAudit() {
		if r["event"] == EventEnrolled && r["moved_from"] == d1.Key.Tenant && r["tenant"] == "default" {
			moved = true
		}
	}
	if !moved {
		t.Error("no enrollment audit row naming the move")
	}
	// The key is the team's now: a hello asking for an anonymous session is admitted as §5 says, the field ignored.
	d3, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113.7")
	if err != nil || d3.Anonymous != nil || d3.Key.TenantID != e.team {
		t.Fatalf("after the conversion: %+v %v", d3, err)
	}
}

// DM4-20, migration 0003: an anon-… slug is refused for an account or a team tenant.
func TestAnonymousSlugIsReserved(t *testing.T) {
	e := newAnonEnv(t, nil)
	for _, kind := range []string{"account", "team"} {
		_, err := e.pool.Exec(context.Background(), `insert into kontra.tenants (id, slug, kind) values ($1, 'anon-0123456789abcdef', $2)`,
			uuid.Must(uuid.NewV7()), kind)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != "anonymous_slug" {
			t.Errorf("an %s tenant with an anon- slug: %v", kind, err)
		}
	}
}

// §2.4, §6 (DM4-18; M4 WP-03 review): on a gate without tunnel.anonymous (the operator turned it off), a key of a
// still-live anonymous tenant is no anonymous session's: a hello asking for one is anonymous_unavailable; any other is
// §5's, as a new key's. So the probe (no field, no tokens, no auth) is unknown_key, which tells the node nothing
// committed; a hire-only auth key with no service is admitted hire-only, the key moved into the API key's tenant and
// the anonymous tenant ended (DM4-19's "converted"); one declaring services that nothing enrolls is unknown_key.
func TestAdmitAnonymousKeyOnAGateWithoutAnonymous(t *testing.T) {
	e := newAnonEnv(t, nil)
	ctx := context.Background()
	k := newTestKey(t)
	d1, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	off := NewStore(e.pool, Options{PublicURL: "https://gate.example", Keys: e.keys, Multi: true})
	admitOff := func(h *tunnel.Hello) (*Decision, error) {
		return off.Admit(ctx, Request{Hello: h, PubKey: k.pub, SessionID: uuid.Must(uuid.NewV7()), ReplicaID: e.replic,
			Client: netip.MustParseAddr("203.0.113.5")})
	}
	_, err = admitOff(hello(k, a2a("hello")))
	wantCode(t, "the probe", err, tunnel.CodeUnknownKey)
	_, err = admitOff(anonHello(k, a2a("hello")))
	wantCode(t, "a hello asking for a session", err, tunnel.CodeAnonymousUnavailable)

	hireOnly, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: e.team, Name: "hirer", Scopes: []auth.Scope{auth.ScopeHire}})
	if err != nil {
		t.Fatal(err)
	}
	withServices := hello(k, a2a("hello"))
	withServices.Auth = hireOnly.Secret
	_, err = admitOff(withServices)
	wantCode(t, "a hire-only auth key declaring services", err, tunnel.CodeUnknownKey)
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2`, k.kid, d1.Key.TenantID); n != 1 {
		t.Fatal("a refused hello moved the key")
	}

	h := hello(k)
	h.Auth = hireOnly.Secret
	d, err := admitOff(h)
	if err != nil {
		t.Fatal(err)
	}
	if d.Anonymous != nil || d.Key.TenantID != e.team || d.Hire == nil || d.HirePrincipal != d.Hire.ID || len(d.Served) != 0 {
		t.Fatalf("hire-only: %+v %+v", d.Key, d.Hire)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2`, k.kid, e.team); n != 1 {
		t.Fatal("the key did not move into the API key's tenant")
	}
	if n := e.count(`select count(*) from kontra.tenants where id = $1 and expires_at <= now()`, d1.Key.TenantID); n != 1 {
		t.Fatal("the anonymous tenant the key left did not end")
	}
	if n := e.count(`select count(*) from kontra.services where tenant_id = $1 and disabled_at is null`, d1.Key.TenantID); n != 0 {
		t.Errorf("%d services of the anonymous tenant still enabled", n)
	}
}

// waitLockWaits waits until n sessions of this database wait on a lock of type lock ("advisory", "transactionid",
// "tuple", ...).
func (e *pgEnv) waitLockWaits(n int, lock string) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for e.count(`select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'
		and wait_event = $1`, lock) < n {
		if time.Now().After(deadline) {
			e.t.Fatalf("%d sessions never waited on a %s lock", n, lock)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// DM4-19, §6.1 (M4 WP-03 review, lock order): a conversion in flight (enroll tokens from a key of a live anonymous
// tenant: the key moved, its anonymous bindings deleted, then the tenant ended) holds the anonymous tenant's row from
// right after the key's, so the expiry sweep, when the tenant reaches its end meanwhile, skips it instead of waiting on
// the conversion's bindings while the conversion waits on the tenant (a 40P01 before). The conversion then ends the
// tenant itself ("converted"), once.
func TestConversionAndExpiryNeverWaitOnEachOther(t *testing.T) {
	e := newAnonEnv(t, nil)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	k := newTestKey(t)
	d1, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	tok := e.token(e.team, "hello")
	// The tenant reaches its end while the conversion runs.
	e.exec(`update kontra.tenants set expires_at = now() + interval '3 seconds' where id = $1`, d1.Key.TenantID)

	// Hold the audit lock: the conversion stops at its first audit row, after the key's move (enroll's write).
	e.store.lockTimeout = 30 * time.Second
	defer func() { e.store.lockTimeout = admitLockTimeout }()
	locker, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close(context.Background())
	held, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(context.Background())
	if _, err := held.Exec(ctx, `select pg_advisory_xact_lock($1)`, audit.LockID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		d   *Decision
		err error
	}
	done := make(chan result, 1)
	go func() {
		h := hello(k, a2a("hello"))
		h.Enroll = []string{tok}
		d, err := e.admitFrom(k, h, "203.0.113.7")
		done <- result{d, err}
	}()
	e.waitLockWaits(1, "advisory")
	deadline := time.Now().Add(20 * time.Second)
	for e.count(`select count(*) from kontra.tenants where id = $1 and expires_at <= now()`, d1.Key.TenantID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the tenant never reached its end")
		}
		time.Sleep(50 * time.Millisecond)
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	n, err := anon.NewSweeper(e.pool, anon.Defaults, nil, nil).Expire(sctx)
	cancel()
	if err != nil || n != 0 {
		t.Errorf("the expiry sweep during the conversion: ended %d, %v (it waited on the conversion)", n, err)
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.d.Key.TenantID != e.team {
		t.Fatalf("the conversion: %v %+v", r.err, r.d)
	}
	reasons := map[any]int{}
	for _, row := range e.verifyAudit() {
		if row["event"] == anon.EventExpired && row["tenant_id"] == d1.Key.TenantID.String() {
			reasons[row["reason"]]++
		}
	}
	if len(reasons) != 1 || reasons["converted"] != 1 {
		t.Fatalf("the anonymous tenant's end: %v, want it converted once", reasons)
	}
	if n, err := anon.NewSweeper(e.pool, anon.Defaults, nil, nil).Expire(ctx); err != nil || n != 0 {
		t.Fatalf("expire after the conversion: %d %v", n, err)
	}
}

// DM4-19, §6.1 (M4 WP-03 review, lock order): the admission locks a key's anonymous tenant even once it has ended, as
// long as the expiry sweep has not run on it. Otherwise enroll tokens from such a key move it (its bindings deleted)
// while an expiry batch that has already written an audit row (the global audit lock) waits on those bindings, and the
// admission then waits on the audit lock. Here the admission is held after the move (the team service's row locked);
// the batch must end the other tenant and skip this one, without waiting.
func TestEnrollFromAnEndedAnonymousKeyAndExpiryNeverWaitOnEachOther(t *testing.T) {
	e := newAnonEnv(t, nil)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	other, k := newTestKey(t), newTestKey(t)
	d1, err := e.admitFrom(other, anonHello(other, a2a("x")), "198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := e.admitFrom(k, anonHello(k, a2a("hello")), "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	// Both past their end, not yet swept; the other one first in the batch.
	e.exec(`update kontra.tenants set expires_at = now() - interval '2 seconds' where id = $1`, d1.Key.TenantID)
	e.exec(`update kontra.tenants set expires_at = now() - interval '1 second' where id = $1`, d2.Key.TenantID)
	tok := e.token(e.team, "hello")

	e.store.lockTimeout = 30 * time.Second
	defer func() { e.store.lockTimeout = admitLockTimeout }()
	locker, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close(context.Background())
	held, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(context.Background())
	// The team service's row: the admission's binding to it waits here, after the key's move.
	if _, err := held.Exec(ctx, `select 1 from kontra.services where tenant_id = $1 and name = 'hello' for update`, e.team); err != nil {
		t.Fatal(err)
	}
	type result struct {
		d   *Decision
		err error
	}
	done := make(chan result, 1)
	go func() {
		h := hello(k, a2a("hello"))
		h.Enroll = []string{tok}
		d, err := e.admitFrom(k, h, "203.0.113.7")
		done <- result{d, err}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for e.count(`select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the admission never waited on the service's row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	n, err := anon.NewSweeper(e.pool, anon.Defaults, nil, nil).Expire(sctx)
	cancel()
	if err != nil || n != 1 {
		t.Errorf("the expiry sweep during the enrollment: ended %d, %v (it waited on the enrollment)", n, err)
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.d.Key.TenantID != e.team {
		t.Fatalf("the enrollment: %v %+v", r.err, r.d)
	}
	if n, err := anon.NewSweeper(e.pool, anon.Defaults, nil, nil).Expire(ctx); err != nil || n != 1 {
		t.Fatalf("expire after the enrollment: %d %v, want the tenant the key left", n, err)
	}
}
