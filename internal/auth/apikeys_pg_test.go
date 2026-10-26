package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tenant"
)

type pgEnv struct {
	dsn    string
	pool   *pgxpool.Pool
	tenant uuid.UUID
}

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	id, err := tenant.EnsureTeam(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return &pgEnv{dsn: dsn, pool: pool, tenant: id}
}

func (e *pgEnv) mint(t *testing.T, k *APIKeys, req MintRequest) Minted {
	t.Helper()
	if req.TenantID == uuid.Nil {
		req.TenantID = e.tenant
	}
	m, err := k.Mint(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Mint stores only sha256 and the prefix; Authenticate maps the key to key:<prefix> with its tenant (and
// slug), buyer and scopes; unknown, expired and revoked keys are 401; each change is an audit row.
func TestMintAndAuthenticate(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	keys := NewAPIKeys(e.pool, nil)
	m := e.mint(t, keys, MintRequest{Name: "demo"})
	if !GateKeyRE.MatchString(m.Secret) || !strings.HasPrefix(m.Secret, "kk_demo_") || m.Prefix != m.Secret[:16] {
		t.Fatalf("minted %q prefix %q", m.Secret, m.Prefix)
	}
	var (
		hash, prefix, name string
		scopes             []string
		tenantID, buyer    uuid.UUID
		rawHash            []byte
	)
	err := e.pool.QueryRow(ctx, `select encode(hash, 'hex'), hash, prefix, name, scopes, tenant_id, buyer_id from kontra.api_keys where id = $1`, m.ID).
		Scan(&hash, &rawHash, &prefix, &name, &scopes, &tenantID, &buyer)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(m.Secret))
	if !bytes.Equal(rawHash, sum[:]) || prefix != m.Prefix || name != "demo" || len(scopes) != 1 || scopes[0] != "hire" || tenantID != e.tenant || buyer != m.BuyerID {
		t.Errorf("row: hash %s prefix %q name %q scopes %v tenant %s buyer %s", hash, prefix, name, scopes, tenantID, buyer)
	}
	// Nothing in the database holds the secret or its random tail.
	var leaks int
	tail := m.Secret[len(m.Prefix):]
	if err := e.pool.QueryRow(ctx, `select count(*) from kontra.api_keys where position($1 in prefix || name) > 0 or position($2 in prefix || name) > 0`,
		m.Secret, tail).Scan(&leaks); err != nil || leaks != 0 {
		t.Errorf("the secret is stored: %d %v", leaks, err)
	}

	p, err := keys.Authenticate(ctx, m.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindKey || p.ID != "key:"+m.Prefix || *p.TenantID != e.tenant || p.TenantSlug != tenant.TeamSlug ||
		p.BuyerID != m.BuyerID || p.Via != ViaAPIKey || *p.KeyID != m.ID || len(p.Scopes) != 1 || p.Scopes[0] != ScopeHire {
		t.Errorf("principal %+v", p)
	}
	if !mustList(t, "org:default").Admits(p) || !mustList(t, "key:kk_demo_*").Admits(p) || mustList(t, "key:kk_other_*").Admits(p) {
		t.Error("allow lists over the authenticated principal")
	}

	for name, bearer := range map[string]string{
		"unknown":       "kk_demo_" + strings.Repeat("A", 32),
		"prefix only":   m.Prefix,
		"one char off":  m.Secret[:len(m.Secret)-1] + string(m.Secret[len(m.Secret)-1]^1),
		"empty":         "",
		"too long":      strings.Repeat("k", MaxBearer+1),
		"secret + junk": m.Secret + "x",
	} {
		if _, err := keys.Authenticate(ctx, bearer); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: %v, want ErrUnauthenticated", name, err)
		}
	}

	// Expired: a key whose expires_at has passed is 401, one in the future is fine.
	past, future := time.Now().Add(-time.Second), time.Now().Add(time.Hour)
	exp := e.mint(t, keys, MintRequest{Name: "old", ExpiresAt: &past})
	if _, err := keys.Authenticate(ctx, exp.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired key: %v", err)
	}
	fut := e.mint(t, keys, MintRequest{Name: "new", ExpiresAt: &future})
	if _, err := keys.Authenticate(ctx, fut.Secret); err != nil {
		t.Errorf("unexpired key: %v", err)
	}

	// Revoked: 401 from the next request; a second revocation changes nothing; an unknown id is an error.
	changed, err := keys.Revoke(ctx, m.ID, "test")
	if err != nil || !changed {
		t.Fatalf("revoke: %v %v", changed, err)
	}
	if _, err := keys.Authenticate(ctx, m.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked key: %v", err)
	}
	if changed, err := keys.Revoke(ctx, m.ID, "test"); err != nil || changed {
		t.Errorf("second revoke: %v %v", changed, err)
	}
	if _, err := keys.Revoke(ctx, uuid.Must(uuid.NewV7()), "test"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("unknown revoke: %v", err)
	}

	// Through the Authenticator: the request's bearer, an anonymous request, and a bad bearer.
	a := NewAuthenticator(keys)
	r := httptest.NewRequest("POST", "/agents/hello", nil)
	r.Header.Set("Authorization", "Bearer "+fut.Secret)
	p, authed, err := a.Authenticate(r, uuid.Nil)
	if err != nil || !authed || p.ID != "key:"+fut.Prefix {
		t.Errorf("Authenticator: %+v %v %v", p, authed, err)
	}
	// On an anonymous service the authenticated caller keeps its own principal, buyer and via.
	if !mustList(t, "anonymous").Admits(p) || p.Kind != KindKey || p.BuyerID != fut.BuyerID || p.Via != ViaAPIKey {
		t.Errorf("on an anonymous service: %+v", p)
	}
	r.Header.Set("Authorization", "Bearer "+m.Secret)
	if _, _, err := a.Authenticate(r, uuid.Nil); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked through the Authenticator: %v", err)
	}

	// The audit chain: four mints and one revocation, linked, with no secret in any row.
	rows, err := audit.Verify(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, row := range rows {
		var f map[string]any
		if err := json.Unmarshal(row.RowJCS, &f); err != nil {
			t.Fatal(err)
		}
		events = append(events, f["event"].(string))
		for _, secret := range []string{m.Secret, exp.Secret, fut.Secret, tail} {
			if bytes.Contains(row.RowJCS, []byte(secret)) {
				t.Errorf("audit row %d holds a secret", row.N)
			}
		}
		if bytes.Contains(row.RowJCS, []byte(hash)) {
			t.Errorf("audit row %d holds a key hash", row.N)
		}
	}
	want := "api_key.minted,api_key.minted,api_key.minted,api_key.revoked"
	if strings.Join(events, ",") != want {
		t.Errorf("audit events %v, want %s", events, want)
	}
	var last map[string]any
	_ = json.Unmarshal(rows[len(rows)-1].RowJCS, &last)
	if last["key_id"] != m.ID.String() || last["principal"] != "key:"+m.Prefix || last["actor"] != "test" || last["tenant_id"] != e.tenant.String() {
		t.Errorf("revocation row %v", last)
	}
}

// Mint validates its request: scopes, operator prefix, name, tenant; scopes are kept as given.
func TestMintRequest(t *testing.T) {
	e := newPGEnv(t)
	keys := NewAPIKeys(e.pool, nil)
	m := e.mint(t, keys, MintRequest{Name: "Node Enroller", Prefix: "acme", Scopes: []Scope{"enroll:hello", ScopeHire, "enroll:hello"}})
	if !strings.HasPrefix(m.Secret, "kk_acme_") || len(m.Scopes) != 2 || m.Scopes[0] != "enroll:hello" || m.Scopes[1] != ScopeHire {
		t.Errorf("minted %q scopes %v", m.Secret, m.Scopes)
	}
	p, err := keys.Authenticate(context.Background(), m.Secret)
	if err != nil || !p.HasScope("enroll:hello") || !p.HasScope(ScopeHire) {
		t.Errorf("principal %+v %v", p, err)
	}
	enrollOnly := e.mint(t, keys, MintRequest{Name: "enroller", Scopes: []Scope{ScopeEnrollAll}})
	p, err = keys.Authenticate(context.Background(), enrollOnly.Secret)
	if err != nil || p.MayHire() || mustList(t, "org:default").Admits(p) {
		t.Errorf("an enroll-only key may call a service: %+v %v", p, err)
	}
	// Two keys minted without a buyer get two buyers; one minted with a buyer shares it.
	shared := e.mint(t, keys, MintRequest{Name: "second", BuyerID: &m.BuyerID})
	if shared.BuyerID != m.BuyerID || enrollOnly.BuyerID == m.BuyerID {
		t.Errorf("buyers: %s %s %s", m.BuyerID, shared.BuyerID, enrollOnly.BuyerID)
	}
	for name, req := range map[string]MintRequest{
		"bad scope":      {TenantID: e.tenant, Name: "x", Scopes: []Scope{"admin"}},
		"bad prefix":     {TenantID: e.tenant, Name: "x", Prefix: "ACME"},
		"empty name":     {TenantID: e.tenant},
		"no tenant":      {Name: "x"},
		"unknown tenant": {TenantID: uuid.Must(uuid.NewV7()), Name: "x"},
	} {
		if _, err := keys.Mint(context.Background(), req); err == nil {
			t.Errorf("%s: minted", name)
		}
	}
}

// A prefix collision mints again (spec §2.3): the first two draws produce the same key, the third a new
// one; after mintAttempts collisions in a row Mint gives up.
func TestMintPrefixCollision(t *testing.T) {
	e := newPGEnv(t)
	keys := NewAPIKeys(e.pool, nil)
	same := bytes.Repeat([]byte{1}, 64)
	keys.rand = bytes.NewReader(append(append(append([]byte{}, same...), same...), bytes.Repeat([]byte{2}, 64)...))
	first := e.mint(t, keys, MintRequest{Name: "demo"})
	second := e.mint(t, keys, MintRequest{Name: "demo"})
	if first.Prefix == second.Prefix || second.Secret != "kk_demo_"+strings.Repeat("2", 32) {
		t.Errorf("after a collision: %q then %q", first.Secret, second.Secret)
	}
	keys.rand = bytes.NewReader(bytes.Repeat([]byte{1}, 64*mintAttempts))
	if _, err := keys.Mint(context.Background(), MintRequest{TenantID: e.tenant, Name: "demo"}); err == nil || !strings.Contains(err.Error(), "collided") {
		t.Errorf("five collisions: %v", err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `select count(*) from kontra.api_keys`).Scan(&n)
	var a int
	_ = e.pool.QueryRow(context.Background(), `select count(*) from kontra.audit`).Scan(&a)
	if n != 2 || a != 2 {
		t.Errorf("%d keys and %d audit rows, want 2 and 2 (a collided mint leaves nothing)", n, a)
	}
}

// last_used_at is written at most once a minute: the first use writes it, uses within the minute do not,
// a use after it does; the update's own guard holds across replicas.
func TestLastUsedAtOncePerMinute(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	keys := NewAPIKeys(e.pool, nil)
	m := e.mint(t, keys, MintRequest{Name: "demo"})
	read := func() *time.Time {
		var at *time.Time
		if err := e.pool.QueryRow(ctx, `select last_used_at from kontra.api_keys where id = $1`, m.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if read() != nil {
		t.Fatal("last_used_at set at mint")
	}
	clock := time.Now()
	keys.now = func() time.Time { return clock }
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Fatal(err)
	}
	keys.Wait()
	first := read()
	if first == nil {
		t.Fatal("first use did not write last_used_at")
	}
	// Within the minute: this replica does not write again (no statement), whatever the column says.
	if _, err := e.pool.Exec(ctx, `update kontra.api_keys set last_used_at = now() - interval '2 minutes' where id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	backdated := read()
	clock = clock.Add(59 * time.Second)
	for i := 0; i < 5; i++ {
		if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
			t.Fatal(err)
		}
	}
	keys.Wait()
	if got := read(); !got.Equal(*backdated) {
		t.Errorf("a use within the minute wrote last_used_at: %v → %v", backdated, got)
	}
	// After the minute it writes again.
	clock = clock.Add(2 * time.Second)
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Fatal(err)
	}
	keys.Wait()
	if got := read(); !got.After(*backdated) {
		t.Errorf("a use after the minute did not write: %v", got)
	}
	// Another replica (its own memory) within the minute of the column: the guard keeps it.
	written := read()
	other := NewAPIKeys(e.pool, nil)
	if _, err := other.Authenticate(ctx, m.Secret); err != nil {
		t.Fatal(err)
	}
	other.Wait()
	if got := read(); !got.Equal(*written) {
		t.Errorf("a second replica rewrote last_used_at within the minute: %v → %v", written, got)
	}
}

// A key of a deleted or expired tenant is 401, also when its verified answer is held in memory: the
// tenant's expiry ends the cached answer when it passes, and a deletion ends it at the next re-read. Every
// assertion runs within CacheTTL, so the answers come from memory unless the row is read.
func TestKeyOfDeletedTenant(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	rc := NewRevocationCache(e.pool)
	rc.Catchup()
	keys := NewAPIKeys(e.pool, rc)
	m := e.mint(t, keys, MintRequest{Name: "demo"})
	if _, err := e.pool.Exec(ctx, `update kontra.tenants set expires_at = now() + interval '2 minutes' where id = $1`, e.tenant); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	keys.now = func() time.Time { return clock }
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(3 * time.Minute) // past the tenant's expiry, within CacheTTL
	if _, err := keys.Authenticate(ctx, m.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("cached key past its tenant's expiry: %v", err)
	}

	keys.now = time.Now
	if _, err := e.pool.Exec(ctx, `update kontra.tenants set expires_at = null where id = $1`, e.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Fatalf("after the expiry is lifted: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `update kontra.tenants set deleted_at = now() where id = $1`, e.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Errorf("before any re-read the cached answer stands: %v", err)
	}
	rc.Catchup()
	if _, err := keys.Authenticate(ctx, m.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("cached key of a deleted tenant after a re-read: %v", err)
	}
	keys.Wait()
}

// A verified key held in memory stops authenticating at the next re-read whenever its row or its tenant
// says it should, not only when its revoked_at is recent: a deleted tenant, a tenant or key expiry moved
// into the past, a revoked_at stamped before the re-read's overlap, a deleted row (spec §2.3: a revoked or
// expired key is 401 on its next request; the re-read runs at least every RefreshInterval).
func TestCachedKeyRevalidated(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	rc := NewRevocationCache(e.pool)
	rc.Catchup()
	keys := NewAPIKeys(e.pool, rc)
	for i, tc := range []struct {
		name, sql string
		byTenant  bool
	}{
		{"tenant deleted", `update kontra.tenants set deleted_at = now() where id = $1`, true},
		{"tenant expired", `update kontra.tenants set expires_at = now() - interval '1 second' where id = $1`, true},
		{"key expired", `update kontra.api_keys set expires_at = now() - interval '1 second' where id = $1`, false},
		{"revoked as of 2 minutes ago", `update kontra.api_keys set revoked_at = now() - interval '2 minutes' where id = $1`, false},
		{"revoked as of a day ago", `update kontra.api_keys set revoked_at = now() - interval '1 day' where id = $1`, false},
		{"row deleted", `delete from kontra.api_keys where id = $1`, false},
	} {
		tenantID := uuid.Must(uuid.NewV7())
		if _, err := e.pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind) values ($1, $2, 'account')`,
			tenantID, fmt.Sprintf("acct%d", i)); err != nil {
			t.Fatal(err)
		}
		m := e.mint(t, keys, MintRequest{TenantID: tenantID, Name: "demo"})
		if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		arg := m.ID
		if tc.byTenant {
			arg = tenantID
		}
		if _, err := e.pool.Exec(ctx, tc.sql, arg); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
			t.Errorf("%s: before any re-read the cached answer stands (the cache is in use): %v", tc.name, err)
		}
		rc.Catchup()
		for n := 0; n < 2; n++ {
			if _, err := keys.Authenticate(ctx, m.Secret); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("%s: request %d after a re-read: %v, want ErrUnauthenticated", tc.name, n+1, err)
			}
		}
	}
	keys.Wait()
}

// The key cache is trusted for RefreshInterval after the last good re-read, not longer: when re-reads
// fail, a key revoked behind the replica's back (no NOTIFY) is read from Postgres once 5 s have passed.
func TestStaleAfterRefreshInterval(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	rc := NewRevocationCache(e.pool)
	rc.Catchup()
	keys := NewAPIKeys(e.pool, rc)
	m := e.mint(t, keys, MintRequest{Name: "demo"})
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `update kontra.api_keys set revoked_at = now() where id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Fatalf("within the interval the cached answer stands: %v", err)
	}
	rc.mu.Lock()
	rc.lastOK = rc.lastOK.Add(-RefreshInterval - 100*time.Millisecond) // the re-reads since have failed
	rc.mu.Unlock()
	if rc.Fresh() {
		t.Errorf("Fresh %v after the last good read", RefreshInterval+100*time.Millisecond)
	}
	if _, err := keys.Authenticate(ctx, m.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked key %v after the last good read: %v, want a read of the row", RefreshInterval, err)
	}
	keys.Wait()
}

// slowReads moves a test clock forward when the revocation cache's statements start, as if each took that
// long.
type slowReads struct {
	advance func()
}

func (s slowReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "kontra.api_keys") {
		s.advance()
	}
	return ctx
}

func (slowReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Fresh counts from the start of the last good re-read, whose snapshot is what the cache knows: after a
// re-read that took 4 s, memory is trusted for 1 s more, not for 5.
func TestFreshCountsFromTheReadStart(t *testing.T) {
	dsn := pgtest.Migrated(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	start := time.Now()
	clock := start
	cfg.ConnConfig.Tracer = slowReads{advance: func() { mu.Lock(); clock = clock.Add(4 * time.Second); mu.Unlock() }}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rc := NewRevocationCache(pool)
	rc.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	set := func(d time.Duration) { mu.Lock(); clock = start.Add(d); mu.Unlock() }
	rc.Catchup()
	set(4500 * time.Millisecond)
	if !rc.Fresh() {
		t.Error("not fresh 4.5 s after the read began")
	}
	set(5500 * time.Millisecond)
	if rc.Fresh() {
		t.Error("fresh 5.5 s after the read began (1.5 s after it ended): the bound counts from its end")
	}
}

// replica is one gate replica's authentication: its own pool, revocation cache and (optionally) LISTEN
// connection.
type replica struct {
	keys    *APIKeys
	revoked *RevocationCache
}

func startReplica(t *testing.T, dsn string, listen bool) *replica {
	t.Helper()
	pool := pgtest.Pool(t, dsn)
	rc := NewRevocationCache(pool)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	if listen {
		cfg, err := pg.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		l := pg.NewListener(cfg)
		l.OnChannel(pg.ChannelRevoke, rc.Notify)
		l.OnReconnect(rc.Catchup)
		wg.Add(1)
		go func() { defer wg.Done(); _ = l.Run(ctx) }()
		waitUntil(t, 10*time.Second, l.Healthy)
	}
	wg.Add(1)
	go func() { defer wg.Done(); _ = rc.Run(ctx) }()
	waitUntil(t, 10*time.Second, rc.Fresh)
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return &replica{keys: NewAPIKeys(pool, rc), revoked: rc}
}

func waitUntil(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// timeTo401 polls Authenticate until it is 401 and returns how long that took.
func timeTo401(t *testing.T, k *APIKeys, secret string, start time.Time, limit time.Duration) time.Duration {
	t.Helper()
	for {
		_, err := k.Authenticate(context.Background(), secret)
		if errors.Is(err, ErrUnauthenticated) {
			return time.Since(start)
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > limit {
			t.Fatalf("still authenticated %v after the revocation", time.Since(start))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A revocation on one replica reaches another within 1 s through NOTIFY kontra_revoke, and within 5 s
// (RefreshInterval) when the NOTIFY never arrives; until then the other replica answers the key from
// memory, without Postgres.
func TestRevocationReachesOtherReplicas(t *testing.T) {
	e := newPGEnv(t)
	a := startReplica(t, e.dsn, true)
	b := startReplica(t, e.dsn, true)  // listens
	c := startReplica(t, e.dsn, false) // NOTIFY suppressed: re-reads only
	ctx := context.Background()

	m := e.mint(t, a.keys, MintRequest{Name: "demo"})
	for _, r := range []*replica{a, b, c} {
		if _, err := r.keys.Authenticate(ctx, m.Secret); err != nil {
			t.Fatal(err)
		}
	}
	// b and c now answer the key from memory (TestRevocationCatchupAndStaleness shows they do): what
	// ends it is the NOTIFY (b) or the next re-read (c).
	start := time.Now()
	if _, err := a.keys.Revoke(ctx, m.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.keys.Authenticate(ctx, m.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("the revoking replica: %v", err)
	}
	viaNotify := timeTo401(t, b.keys, m.Secret, start, 5*time.Second)
	viaRead := timeTo401(t, c.keys, m.Secret, start, 10*time.Second)
	t.Logf("revocation reached the listening replica after %v and the non-listening one after %v", viaNotify, viaRead)
	if viaNotify > time.Second {
		t.Errorf("through NOTIFY: %v, want within 1 s", viaNotify)
	}
	if viaRead > RefreshInterval {
		t.Errorf("without NOTIFY: %v, want within %v", viaRead, RefreshInterval)
	}
}

// The verified-key cache is used, and its staleness bounded: a key revoked behind the replica's back
// (SQL, no NOTIFY, no re-read yet) is still answered from memory; Catchup (the LISTEN reconnect) applies
// it at once; a replica whose revocation cache is not current reads Postgres on every request.
func TestRevocationCatchupAndStaleness(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	rc := NewRevocationCache(e.pool)
	rc.Catchup() // one read: fresh, no Run loop
	if !rc.Fresh() {
		t.Fatal("not fresh after a read")
	}
	keys := NewAPIKeys(e.pool, rc)
	m := e.mint(t, keys, MintRequest{Name: "demo"})
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `update kontra.api_keys set revoked_at = now() where id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(ctx, m.Secret); err != nil {
		t.Errorf("served from memory before any re-read: %v (want the cached answer)", err)
	}
	rc.Catchup()
	if _, err := keys.Authenticate(ctx, m.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("after Catchup: %v", err)
	}
	if !rc.APIKeyRevoked(m.ID) {
		t.Error("the re-read did not record the revocation")
	}

	// A revocation cache whose last good read began more than RefreshInterval ago is not trusted: every
	// request reads the row.
	m2 := e.mint(t, keys, MintRequest{Name: "demo2"})
	if _, err := keys.Authenticate(ctx, m2.Secret); err != nil {
		t.Fatal(err)
	}
	rc.mu.Lock()
	rc.lastOK = time.Now().Add(-3 * RefreshInterval)
	rc.mu.Unlock()
	if _, err := e.pool.Exec(ctx, `update kontra.api_keys set revoked_at = now() where id = $1`, m2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(ctx, m2.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("stale revocation cache: %v, want a read of the row", err)
	}

	// A verified key is read again after CacheTTL even when nothing is revoked.
	rc.Catchup()
	m3 := e.mint(t, keys, MintRequest{Name: "demo3"})
	clock := time.Now()
	keys.now = func() time.Time { return clock }
	if _, err := keys.Authenticate(ctx, m3.Secret); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `update kontra.api_keys set expires_at = now() - interval '1 second' where id = $1`, m3.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(ctx, m3.Secret); err != nil {
		t.Errorf("within the TTL the cached row answers: %v", err)
	}
	clock = clock.Add(CacheTTL)
	rc.Catchup()
	if _, err := keys.Authenticate(ctx, m3.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("after the TTL the row is read again: %v", err)
	}
	keys.Wait()
}

// A revocation older than the first read's window is still refused: the key's own row says so.
func TestRevokedBeforeStart(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	keys := NewAPIKeys(e.pool, nil)
	m := e.mint(t, keys, MintRequest{Name: "demo"})
	if _, err := e.pool.Exec(ctx, `update kontra.api_keys set revoked_at = now() - interval '1 day' where id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	rc := NewRevocationCache(e.pool)
	rc.Catchup()
	if rc.APIKeyRevoked(m.ID) {
		t.Error("a day-old revocation is held in memory (only recent ones need to be)")
	}
	if _, err := NewAPIKeys(e.pool, rc).Authenticate(ctx, m.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked a day ago: %v", err)
	}
}
