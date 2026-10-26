package enroll

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
)

// pgEnv is a migrated database with the team tenant, an API key authenticator and a Store over it.
type pgEnv struct {
	t      *testing.T
	dsn    string
	pool   *pgxpool.Pool
	team   uuid.UUID
	keys   *auth.APIKeys
	store  *Store
	replic string
}

func newPGEnv(t *testing.T, opts ...func(*Options)) *pgEnv {
	t.Helper()
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	team, err := tenant.EnsureTeam(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	e := &pgEnv{t: t, dsn: dsn, pool: pool, team: team, keys: auth.NewAPIKeys(pool, nil), replic: "r-test"}
	t.Cleanup(e.keys.Wait)
	o := Options{PublicURL: "https://gate.example", Keys: e.keys}
	for _, f := range opts {
		f(&o)
	}
	e.store = NewStore(pool, o)
	return e
}

func (e *pgEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *pgEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// service inserts a services row as the sync would (source config): backend tunnel unless direct.
func (e *pgEnv) service(tenantID uuid.UUID, name, kind string, direct bool) {
	e.t.Helper()
	backend := `{"tunnel": {}}`
	if direct {
		backend = `{"` + kind + `": {"url": "http://127.0.0.1:9", "allow_private": true}}`
	}
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, $2, $3, $4::jsonb, 'config')`,
		tenantID, name, kind, backend)
}

func (e *pgEnv) tenant(slug string) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(`insert into kontra.tenants (id, slug, kind) values ($1, $2, 'account')`, id, slug)
	return id
}

func (e *pgEnv) token(tenantID uuid.UUID, service string) string {
	e.t.Helper()
	tok, err := e.store.MintServiceToken(context.Background(), tenantID, service, "test")
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// contend runs n calls of fn at once while a separate connection holds the row lock lockSQL takes, and
// releases it only once all n are waiting on a lock in this database: the calls then really race for the
// row, whatever the scheduling. It returns their errors.
func (e *pgEnv) contend(n int, lockSQL string, lockArgs []any, fn func(i int) error) []error {
	e.t.Helper()
	ctx := context.Background()
	// Open the pool's connections first, so the calls do not queue behind connection setup.
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
	// The calls wait on purpose here: give their lock waits room on a loaded host, and restore the bound.
	e.store.lockTimeout = 20 * time.Second
	defer func() { e.store.lockTimeout = admitLockTimeout }()
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
	if tag, err := tx.Exec(ctx, lockSQL, lockArgs...); err != nil || tag.RowsAffected() != 1 {
		e.t.Fatalf("%s: %v %v", lockSQL, tag, err)
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn(i)
		}()
	}
	// Counted on a connection of its own: the calls may hold every connection of the pool.
	watcher, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		e.t.Fatal(err)
	}
	defer watcher.Close(context.Background())
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := watcher.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			e.t.Fatal(err)
		}
		if waiting == n {
			break
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%d of %d calls waiting on the lock", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	wg.Wait()
	return errs
}

func a2a(name string) tunnel.Service { return tunnel.Service{Kind: tunnel.KindA2A, Name: name} }
func mcp(name string) tunnel.Service { return tunnel.Service{Kind: tunnel.KindMCP, Name: name} }

func hello(k *testKey, services ...tunnel.Service) *tunnel.Hello {
	return &tunnel.Hello{T: tunnel.MsgHello, V: tunnel.Version, Origin: "https://gate.example", KID: k.kid,
		Recipient: k.recipient, Name: "node-" + k.kid[3:9], Services: services, Agent: "kontra/test"}
}

func (e *pgEnv) admit(k *testKey, h *tunnel.Hello, old *testKey) (*Decision, error) {
	req := Request{Hello: h, PubKey: k.pub, SessionID: uuid.Must(uuid.NewV7()), ReplicaID: e.replic}
	if old != nil {
		h.RotateFrom = old.ref()
		req.OldPubKey = old.pub
	}
	return e.store.Admit(context.Background(), req)
}

func (e *pgEnv) mustAdmit(k *testKey, h *tunnel.Hello) *Decision {
	e.t.Helper()
	d, err := e.admit(k, h, nil)
	if err != nil {
		e.t.Fatalf("admit %s: %v", k.kid, err)
	}
	return d
}

func wantCode(t *testing.T, what string, err error, code tunnel.Code) {
	t.Helper()
	var herr *tunnel.HandshakeError
	if !errors.As(err, &herr) || herr.Code != code {
		t.Fatalf("%s: %v, want %s", what, err, code)
	}
}

func served(d *Decision) []string {
	var out []string
	for _, s := range d.Served {
		out = append(out, s.Kind+":"+s.Name+"@"+s.URL)
	}
	return out
}

// verifyAudit checks the chain and returns the events in order.
func (e *pgEnv) verifyAudit() []map[string]any {
	e.t.Helper()
	rows, err := audit.Verify(context.Background(), e.pool)
	if err != nil {
		e.t.Fatalf("audit: %v", err)
	}
	var out []map[string]any
	for _, r := range rows {
		var m map[string]any
		if err := json.Unmarshal(r.RowJCS, &m); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func events(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r["event"].(string))
	}
	return out
}

// `kontra-gate enroll <service>`'s store call: a token is ket_ and 32 base62 characters, the row holds its
// sha256 and expires 15 minutes after it was created, the mint is an audit row that never carries the
// token; an unknown, disabled or non-tunnel service is refused and writes nothing.
func TestMintServiceToken(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "direct", "a2a", true)
	e.service(e.team, "gone", "a2a", false)
	e.exec(`update kontra.services set disabled_at = now() where name = 'gone'`)

	tok, m, err := e.store.MintServiceTokenInfo(ctx, e.team, "hello", "kontra-gate enroll")
	if err != nil {
		t.Fatal(err)
	}
	if !tokenShape.MatchString(tok) || m.Tenant != tenant.TeamSlug {
		t.Fatalf("token %q, minted %+v", tok, m)
	}
	var (
		hash               []byte
		scope, svc         string
		created, expires   time.Time
		used               *time.Time
		bind               []string
		tokenTenant, rowID uuid.UUID
	)
	err = e.pool.QueryRow(ctx, `select id, tenant_id, scope, service, bind_services, token_hash, created_at, expires_at, used_at
		from kontra.enrollment_tokens`).Scan(&rowID, &tokenTenant, &scope, &svc, &bind, &hash, &created, &expires, &used)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok))
	if !bytes.Equal(hash, sum[:]) || scope != ScopeService || svc != "hello" || bind != nil || used != nil ||
		tokenTenant != e.team || rowID != m.ID {
		t.Errorf("row: scope %s service %s bind %v used %v", scope, svc, bind, used)
	}
	if !expires.Equal(created.Add(TokenTTL)) || !expires.Equal(m.ExpiresAt) {
		t.Errorf("created %s expires %s, want 15 minutes later", created, expires)
	}
	ev := e.verifyAudit()
	if len(ev) != 1 || ev[0]["event"] != EventTokenMinted || ev[0]["service"] != "hello" || ev[0]["scope"] != ScopeService ||
		ev[0]["token_id"] != m.ID.String() || ev[0]["actor"] != "kontra-gate enroll" {
		t.Errorf("audit %v", ev)
	}
	rowsJCS := e.count(`select count(*) from kontra.audit where position($1::bytea in row_jcs) > 0`, []byte(tok[4:]))
	if rowsJCS != 0 {
		t.Error("the audit row carries the token")
	}

	for svc, want := range map[string]error{"nope": ErrUnknownService, "gone": ErrServiceDisabled, "direct": ErrNotTunnel,
		"Not_A_Name": ErrUnknownService} {
		if tok, err := e.store.MintServiceToken(ctx, e.team, svc, ""); !errors.Is(err, want) || tok != "" {
			t.Errorf("mint for %s: %q %v, want %v", svc, tok, err, want)
		}
	}
	other := e.tenant("acme")
	if _, err := e.store.MintServiceToken(ctx, other, "hello", ""); !errors.Is(err, ErrUnknownService) {
		t.Errorf("another tenant's service: %v", err)
	}
	if n := e.count(`select count(*) from kontra.enrollment_tokens`); n != 1 {
		t.Errorf("%d token rows after the refusals, want 1", n)
	}
	if n := len(e.verifyAudit()); n != 1 {
		t.Errorf("%d audit rows after the refusals, want 1", n)
	}
}

// A service token enrolls a new key: tunnel_keys in the token's tenant, key_services, used_at and
// used_by_key, an audit row, and the welcome's services. It is single use, and an expired token is refused.
// The key then reconnects with nothing but the proof.
func TestAdmitServiceToken(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	tok := e.token(e.team, "hello")
	k := newTestKey(t)
	h := hello(k, a2a("hello"))
	h.Enroll = []string{tok}
	d := e.mustAdmit(k, h)
	if got := served(d); !slices.Equal(got, []string{"a2a:hello@https://gate.example/agents/hello"}) {
		t.Errorf("served %v", got)
	}
	if d.Key.KeyID != k.kid || d.Key.TenantID != e.team || d.Key.Tenant != "default" || !bytes.Equal(d.Key.PubKey, k.pub) ||
		d.HirePrincipal != "node:"+k.kid || d.Enrolled != nil || d.Rotated != nil {
		t.Errorf("decision %+v", d)
	}
	var (
		keyTenant uuid.UUID
		pub       []byte
		name      string
		usedAt    *time.Time
		usedBy    *string
	)
	if err := e.pool.QueryRow(ctx, `select tenant_id, pubkey, node_name from kontra.tunnel_keys where key_id = $1`, k.kid).
		Scan(&keyTenant, &pub, &name); err != nil {
		t.Fatal(err)
	}
	if keyTenant != e.team || !bytes.Equal(pub, k.pub) || name != h.Name {
		t.Errorf("tunnel_keys: tenant %s name %q", keyTenant, name)
	}
	if n := e.count(`select count(*) from kontra.key_services where key_id = $1 and service = 'hello' and tenant_id = $2 and revoked_at is null`,
		k.kid, e.team); n != 1 {
		t.Errorf("%d key_services rows", n)
	}
	if err := e.pool.QueryRow(ctx, `select used_at, used_by_key from kontra.enrollment_tokens`).Scan(&usedAt, &usedBy); err != nil {
		t.Fatal(err)
	}
	if usedAt == nil || usedBy == nil || *usedBy != k.kid {
		t.Errorf("token used_at %v used_by_key %v", usedAt, usedBy)
	}
	ev := e.verifyAudit()
	last := ev[len(ev)-1]
	if last["event"] != EventEnrolled || last["key_id"] != k.kid || last["new_key"] != true || last["tenant"] != "default" {
		t.Errorf("audit %v", last)
	}
	if svcs, _ := last["services"].([]any); len(svcs) != 1 || svcs[0].(map[string]any)["via"] != viaServiceToken {
		t.Errorf("audit services %v", last["services"])
	}

	// The same token again, from another key: enroll_invalid, and that key is not written.
	k2 := newTestKey(t)
	h2 := hello(k2, a2a("hello"))
	h2.Enroll = []string{tok}
	_, err := e.admit(k2, h2, nil)
	wantCode(t, "a used token", err, tunnel.CodeEnrollInvalid)
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1`, k2.kid); n != 0 {
		t.Error("a refused key was written")
	}

	// An expired token (15 minutes after it was minted).
	tok3 := e.token(e.team, "hello")
	h3 := TokenHash(tok3)
	e.exec(`update kontra.enrollment_tokens set created_at = now() - interval '16 minutes', expires_at = now() - interval '1 minute' where token_hash = $1`, h3[:])
	k3 := newTestKey(t)
	h3hello := hello(k3, a2a("hello"))
	h3hello.Enroll = []string{tok3}
	_, err = e.admit(k3, h3hello, nil)
	wantCode(t, "an expired token", err, tunnel.CodeEnrollInvalid)
	// A token of the wrong shape and one never minted.
	for _, bad := range []string{"ket_short", "ket_" + strings.Repeat("Z", 32)} {
		hb := hello(k3, a2a("hello"))
		hb.Enroll = []string{bad}
		_, err = e.admit(k3, hb, nil)
		wantCode(t, "token "+bad, err, tunnel.CodeEnrollInvalid)
	}

	// Reconnecting: the proof alone, and a new node name is recorded.
	again := hello(k, a2a("hello"))
	again.Name = "renamed"
	d2 := e.mustAdmit(k, again)
	if got := served(d2); len(got) != 1 || d2.Key.NodeName != "renamed" {
		t.Errorf("reconnect: served %v name %q", got, d2.Key.NodeName)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where node_name = 'renamed'`); n != 1 {
		t.Error("node name not updated")
	}
	// A known key declaring nothing (a node that only hires) is admitted with nothing served.
	if d := e.mustAdmit(k, hello(k)); len(d.Served) != 0 {
		t.Errorf("no services declared: served %v", served(d))
	}
	// A token for a service already bound is consumed, audited, and changes nothing.
	tok4 := e.token(e.team, "hello")
	h4 := hello(k, a2a("hello"))
	h4.Enroll = []string{tok4}
	e.mustAdmit(k, h4)
	if n := e.count(`select count(*) from kontra.enrollment_tokens where used_by_key = $1`, k.kid); n != 2 {
		t.Errorf("%d tokens used by the key, want 2", n)
	}
	e.verifyAudit()
}

// Of 20 concurrent handshakes presenting one token, exactly one commits: from 20 keys, and from one key.
func TestAdmitConcurrentToken(t *testing.T) {
	e := newPGEnv(t)
	e.service(e.team, "hello", "a2a", false)
	for _, sameKey := range []bool{false, true} {
		tok := e.token(e.team, "hello")
		th := TokenHash(tok)
		shared := newTestKey(t)
		keys := make([]*testKey, 20)
		for i := range keys {
			keys[i] = shared
			if !sameKey {
				keys[i] = newTestKey(t)
			}
		}
		admitted := make([]string, 20)
		errs := e.contend(20, `select 1 from kontra.enrollment_tokens where token_hash = $1 for update`, []any{th[:]}, func(i int) error {
			h := hello(keys[i], a2a("hello"))
			h.Enroll = []string{tok}
			d, err := e.admit(keys[i], h, nil)
			if err == nil {
				admitted[i] = d.Key.KeyID
			}
			return err
		})
		ok, winner := 0, ""
		for i, err := range errs {
			if err == nil {
				ok++
				winner = admitted[i]
				continue
			}
			wantCode(t, "a losing handshake", err, tunnel.CodeEnrollInvalid)
		}
		if ok != 1 {
			t.Fatalf("same key %v: %d admitted, want 1", sameKey, ok)
		}
		if n := e.count(`select count(*) from kontra.enrollment_tokens where token_hash = $1 and used_by_key = $2`, th[:], winner); n != 1 {
			t.Errorf("same key %v: the token is not used by the admitted key", sameKey)
		}
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys`); n != 2 {
		t.Errorf("%d keys, want 2 (one per round)", n)
	}
	if n := e.count(`select count(*) from kontra.key_services`); n != 2 {
		t.Errorf("%d bindings, want 2", n)
	}
	e.verifyAudit()
}

// The refusals of §4/§5 and their precedence; a refused handshake writes nothing.
func TestAdmitRefusals(t *testing.T) {
	e := newPGEnv(t)
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	snapshot := func() [4]int {
		return [4]int{
			e.count(`select count(*) from kontra.tunnel_keys`),
			e.count(`select count(*) from kontra.key_services`),
			e.count(`select count(*) from kontra.enrollment_tokens where used_at is not null`),
			e.count(`select count(*) from kontra.audit`),
		}
	}

	t.Run("kind_mismatch", func(t *testing.T) {
		before := snapshot()
		k := newTestKey(t)
		h := hello(k, mcp("hello"))
		h.Enroll = []string{e.token(e.team, "hello")}
		_, err := e.admit(k, h, nil)
		wantCode(t, "a2a token declared as mcp", err, tunnel.CodeKindMismatch)
		if after := snapshot(); after[0] != before[0] || after[1] != before[1] || after[2] != before[2] || after[3] != before[3]+1 {
			t.Errorf("rows %v → %v (only the mint's audit row may be new)", before, after)
		}
	})

	t.Run("service_denied writes nothing", func(t *testing.T) {
		k := newTestKey(t)
		h := hello(k, a2a("hello"), a2a("recon"))
		h.Enroll = []string{e.token(e.team, "hello")}
		before := snapshot()
		_, err := e.admit(k, h, nil)
		wantCode(t, "recon has neither a binding nor an authority", err, tunnel.CodeServiceDenied)
		if after := snapshot(); after != before {
			t.Errorf("rows %v → %v", before, after)
		}
		// The token was not consumed: the same hello declaring only hello succeeds with it.
		h.Services = h.Services[:1]
		if d := e.mustAdmit(k, h); len(d.Served) != 1 {
			t.Errorf("served %v", served(d))
		}
	})

	t.Run("enroll_invalid before service_denied", func(t *testing.T) {
		// key_scope step 2 (M2 plan audit): a consumed token and an undeclared-for service.
		tok := e.token(e.team, "hello")
		k := newTestKey(t)
		h := hello(k, a2a("hello"))
		h.Enroll = []string{tok}
		e.mustAdmit(k, h)
		k2 := newTestKey(t)
		h2 := hello(k2, a2a("hello"), a2a("recon"))
		h2.Enroll = []string{tok}
		_, err := e.admit(k2, h2, nil)
		wantCode(t, "a used token and an unbound service", err, tunnel.CodeEnrollInvalid)
		// The enrolled key declaring an extra service with no token: service_denied.
		_, err = e.admit(k, hello(k, a2a("hello"), a2a("recon")), nil)
		wantCode(t, "a known key and an unbound service", err, tunnel.CodeServiceDenied)
	})

	t.Run("a token for an undeclared service", func(t *testing.T) {
		tok := e.token(e.team, "recon")
		k := newTestKey(t)
		h := hello(k, a2a("hello"))
		h.Enroll = []string{tok}
		_, err := e.admit(k, h, nil)
		wantCode(t, "token for recon, declaring hello", err, tunnel.CodeEnrollInvalid)
		th := TokenHash(tok)
		if n := e.count(`select count(*) from kontra.enrollment_tokens where token_hash = $1 and used_at is null`, th[:]); n != 1 {
			t.Error("the token was consumed")
		}
	})

	t.Run("another tenant's token", func(t *testing.T) {
		acme := e.tenant("acme")
		e.service(acme, "shop", "a2a", false)
		k := newTestKey(t)
		h := hello(k, a2a("hello"))
		h.Enroll = []string{e.token(e.team, "hello")}
		e.mustAdmit(k, h)
		h2 := hello(k, a2a("hello"), a2a("shop"))
		h2.Enroll = []string{e.token(acme, "shop")}
		_, err := e.admit(k, h2, nil)
		wantCode(t, "a key of tenant default with a token of tenant acme", err, tunnel.CodeEnrollInvalid)
		if n := e.count(`select count(*) from kontra.key_services where key_id = $1`, k.kid); n != 1 {
			t.Error("the other tenant's binding was written")
		}
		// A new key with tokens of two tenants.
		k2 := newTestKey(t)
		h3 := hello(k2, a2a("hello"), a2a("shop"))
		h3.Enroll = []string{e.token(e.team, "hello"), e.token(acme, "shop")}
		_, err = e.admit(k2, h3, nil)
		wantCode(t, "tokens of two tenants", err, tunnel.CodeEnrollInvalid)
	})

	t.Run("unknown_key", func(t *testing.T) {
		k := newTestKey(t)
		_, err := e.admit(k, hello(k, a2a("hello")), nil)
		wantCode(t, "an unknown key without enroll or auth", err, tunnel.CodeUnknownKey)
		_, err = e.admit(k, hello(k), nil)
		wantCode(t, "an unknown key declaring nothing", err, tunnel.CodeUnknownKey)
	})

	t.Run("bad_proof", func(t *testing.T) {
		a, b := newTestKey(t), newTestKey(t)
		// The gate stores a's key id with b's public key.
		e.exec(`insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ($1, $2, $3, 'x')`, a.kid, e.team, b.pub)
		_, err := e.admit(a, hello(a, a2a("hello")), nil)
		wantCode(t, "a known key id with another stored public key", err, tunnel.CodeBadProof)
		_, err = e.admit(b, hello(b, a2a("hello")), nil)
		wantCode(t, "a known public key under another key id", err, tunnel.CodeBadProof)
		// The proven key is not the hello's recipient.
		c := newTestKey(t)
		_, err = e.store.Admit(context.Background(), Request{Hello: hello(c), PubKey: a.pub})
		wantCode(t, "a request whose public key is not the hello's", err, tunnel.CodeBadProof)
	})

	t.Run("revoked", func(t *testing.T) {
		k := newTestKey(t)
		h := hello(k, a2a("hello"))
		h.Enroll = []string{e.token(e.team, "hello")}
		e.mustAdmit(k, h)
		if err := e.store.RevokeKey(context.Background(), k.kid, ReasonOperator, "test"); err != nil {
			t.Fatal(err)
		}
		_, err := e.admit(k, hello(k, a2a("hello")), nil)
		wantCode(t, "a revoked key", err, tunnel.CodeRevoked)
		// Revoked wins over a valid token too.
		h2 := hello(k, a2a("hello"))
		h2.Enroll = []string{e.token(e.team, "hello")}
		_, err = e.admit(k, h2, nil)
		wantCode(t, "a revoked key with a token", err, tunnel.CodeRevoked)
	})
	e.verifyAudit()
}

// A revoked binding is left out of the served set and the handshake succeeds for the others; so is a bound
// service that is no longer an enabled tunnel service (M2 plan audit on C10). A new token for the service
// enrolls it again.
func TestAdmitLeavesOutRevokedBindings(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	for _, n := range []string{"hello", "recon", "gone", "moved"} {
		e.service(e.team, n, "a2a", false)
	}
	k := newTestKey(t)
	h := hello(k, a2a("hello"), a2a("recon"), a2a("gone"), a2a("moved"))
	h.Enroll = []string{e.token(e.team, "hello"), e.token(e.team, "recon"), e.token(e.team, "gone"), e.token(e.team, "moved")}
	if d := e.mustAdmit(k, h); len(d.Served) != 4 {
		t.Fatalf("served %v", served(d))
	}
	if err := e.store.RevokeBinding(ctx, k.kid, "recon", "test"); err != nil {
		t.Fatal(err)
	}
	e.exec(`update kontra.services set disabled_at = now() where name = 'gone'`)
	e.exec(`update kontra.services set backend = '{"a2a": {"url": "http://127.0.0.1:9"}}' where name = 'moved'`)
	d := e.mustAdmit(k, hello(k, a2a("hello"), a2a("recon"), a2a("gone"), a2a("moved")))
	if got := served(d); !slices.Equal(got, []string{"a2a:hello@https://gate.example/agents/hello"}) {
		t.Errorf("served %v, want hello alone", got)
	}
	// A bound service declared with another kind is kind_mismatch.
	_, err := e.admit(k, hello(k, mcp("hello")), nil)
	wantCode(t, "a bound a2a service declared as mcp", err, tunnel.CodeKindMismatch)
	// A new token enrolls the revoked binding again.
	h2 := hello(k, a2a("hello"), a2a("recon"))
	h2.Enroll = []string{e.token(e.team, "recon")}
	d = e.mustAdmit(k, h2)
	if got := served(d); len(got) != 2 {
		t.Errorf("after re-enrolling recon: served %v", got)
	}
	if n := e.count(`select count(*) from kontra.key_services where key_id = $1 and service = 'recon' and revoked_at is null`, k.kid); n != 1 {
		t.Error("recon's binding was not enrolled again")
	}
	e.verifyAudit()
}

// A tenant token creates a missing service (kind from the hello, backend tunnel, allow [org:<tenant>], source
// db, NOTIFY kontra_services) and binds it; it never adds a key to a service that already has one; a name
// outside its list is service_denied.
func TestAdmitTenantToken(t *testing.T) {
	e := newPGEnv(t, func(o *Options) { o.Multi = true })
	ctx := context.Background()
	acme := e.tenant("acme")
	e.service(acme, "existing", "a2a", false)
	e.service(acme, "direct", "a2a", true)

	if _, err := e.store.MintTenantToken(ctx, acme, []string{"shop", "direct"}, "cloud"); !errors.Is(err, ErrNotTunnel) {
		t.Errorf("a tenant token listing a direct service: %v", err)
	}
	if _, err := e.store.MintTenantToken(ctx, uuid.New(), []string{"shop"}, "cloud"); !errors.Is(err, ErrUnknownTenant) {
		t.Errorf("an unknown tenant: %v", err)
	}
	if _, err := e.store.MintTenantToken(ctx, acme, nil, "cloud"); err == nil {
		t.Error("a tenant token with no services")
	}
	tok, err := e.store.MintTenantToken(ctx, acme, []string{"shop", "crm", "existing", "shop"}, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	var bind []string
	if err := e.pool.QueryRow(ctx, `select bind_services from kontra.enrollment_tokens where scope = 'tenant'`).Scan(&bind); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bind, []string{"shop", "crm", "existing"}) {
		t.Errorf("bind_services %v", bind)
	}

	listen, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer listen.Close(context.Background())
	if _, err := listen.Exec(ctx, "listen kontra_services"); err != nil {
		t.Fatal(err)
	}

	k := newTestKey(t)
	h := hello(k, a2a("shop"), mcp("crm"), a2a("existing"))
	h.Enroll = []string{tok}
	d := e.mustAdmit(k, h)
	want := []string{"a2a:shop@https://gate.example/agents/acme/shop", "mcp:crm@https://gate.example/mcp/acme/crm",
		"a2a:existing@https://gate.example/agents/acme/existing"}
	if got := served(d); !slices.Equal(got, want) {
		t.Errorf("served %v, want %v", got, want)
	}
	if d.Key.TenantID != acme || d.Key.Tenant != "acme" || len(d.Enrolled) != 0 {
		t.Errorf("decision %+v", d)
	}
	for name, kind := range map[string]string{"shop": "a2a", "crm": "mcp"} {
		var (
			gotKind, source string
			backend         []byte
			allow           []string
		)
		err := e.pool.QueryRow(ctx, `select kind, backend::text, allow, source from kontra.services where tenant_id = $1 and name = $2`,
			acme, name).Scan(&gotKind, &backend, &allow, &source)
		if err != nil {
			t.Fatalf("service %s: %v", name, err)
		}
		if gotKind != kind || string(backend) != `{"tunnel": {}}` || !slices.Equal(allow, []string{"org:acme"}) || source != "db" {
			t.Errorf("service %s: kind %s backend %s allow %v source %s", name, gotKind, backend, allow, source)
		}
	}
	got := map[string]bool{}
	for len(got) < 2 {
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		n, err := listen.WaitForNotification(wctx)
		cancel()
		if err != nil {
			t.Fatalf("NOTIFY kontra_services: got %v, %v", got, err)
		}
		got[n.Payload] = true
	}
	if !got["acme/shop"] || !got["acme/crm"] {
		t.Errorf("notifications %v", got)
	}
	ev := events(e.verifyAudit())
	if n := slices.Index(ev, EventServiceCreated); n < 0 || strings.Count(strings.Join(ev, " "), EventServiceCreated) != 2 ||
		ev[len(ev)-1] != EventEnrolled {
		t.Errorf("audit events %v", ev)
	}

	// A second node with a tenant token cannot join a service that has a key; nothing is written.
	tok2, err := e.store.MintTenantToken(ctx, acme, []string{"shop"}, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	k2 := newTestKey(t)
	h2 := hello(k2, a2a("shop"))
	h2.Enroll = []string{tok2}
	_, err = e.admit(k2, h2, nil)
	wantCode(t, "a tenant token for a served service", err, tunnel.CodeServiceDenied)
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1`, k2.kid); n != 0 {
		t.Error("the refused key was written")
	}
	// A name outside the token's list.
	tok3, err := e.store.MintTenantToken(ctx, acme, []string{"blog"}, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	h3 := hello(k2, a2a("blog"), a2a("wiki"))
	h3.Enroll = []string{tok3}
	_, err = e.admit(k2, h3, nil)
	wantCode(t, "a name outside bind_services", err, tunnel.CodeServiceDenied)
	if n := e.count(`select count(*) from kontra.services where name in ('blog', 'wiki')`); n != 0 {
		t.Error("a refused handshake created a service")
	}
	// A tenant token whose list holds none of the declared services.
	h4 := hello(k2, a2a("wiki"))
	h4.Enroll = []string{tok3}
	_, err = e.admit(k2, h4, nil)
	wantCode(t, "a tenant token binding none of the declared", err, tunnel.CodeEnrollInvalid)
	// A tenant token from another tenant than the key's.
	h5 := hello(k, a2a("shop"))
	tokTeam, err := e.store.MintTenantToken(ctx, e.team, []string{"shop"}, "cloud")
	if err != nil {
		t.Fatal(err)
	}
	h5.Enroll = []string{tokTeam}
	_, err = e.admit(k, h5, nil)
	wantCode(t, "a key of acme with a tenant token of default", err, tunnel.CodeEnrollInvalid)
	e.verifyAudit()
}

// API keys: auth_invalid for an unknown, revoked or expired key; enroll: scopes enroll only with
// tunnel.enroll_with_api_key; the hiring principal is the API key's, else node:<key_id>.
func TestAdmitAPIKey(t *testing.T) {
	ctx := context.Background()
	for _, withScopes := range []bool{false, true} {
		e := newPGEnv(t, func(o *Options) { o.EnrollWithAPIKey = withScopes })
		e.service(e.team, "hello", "a2a", false)
		e.service(e.team, "recon", "a2a", false)
		mint := func(scopes ...auth.Scope) auth.Minted {
			m, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: e.team, Name: "node", Scopes: scopes})
			if err != nil {
				t.Fatal(err)
			}
			return m
		}
		enrollHello := mint(auth.ScopeHire, auth.EnrollScope("hello"))
		all := mint(auth.ScopeEnrollAll)
		hire := mint(auth.ScopeHire)
		revoked := mint(auth.ScopeEnrollAll)
		if _, err := e.keys.Revoke(ctx, revoked.ID, "test"); err != nil {
			t.Fatal(err)
		}
		expired := mint(auth.ScopeEnrollAll)
		e.exec(`update kontra.api_keys set expires_at = now() - interval '1 second' where id = $1`, expired.ID)

		// auth_invalid, before anything is enrolled.
		for name, key := range map[string]string{"unknown": "kk_x_" + strings.Repeat("a", 32), "revoked": revoked.Secret,
			"expired": expired.Secret} {
			k := newTestKey(t)
			h := hello(k, a2a("hello"))
			h.Auth = key
			h.Enroll = []string{e.token(e.team, "hello")}
			_, err := e.admit(k, h, nil)
			wantCode(t, name+" API key", err, tunnel.CodeAuthInvalid)
		}

		// A known key (enrolled for hello by a token).
		known := newTestKey(t)
		hk := hello(known, a2a("hello"))
		hk.Enroll = []string{e.token(e.team, "hello")}
		e.mustAdmit(known, hk)

		// The hiring principal.
		hh := hello(known, a2a("hello"))
		hh.Auth = hire.Secret
		if d := e.mustAdmit(known, hh); d.HirePrincipal != hire.Principal() {
			t.Errorf("hire principal %q, want %q", d.HirePrincipal, hire.Principal())
		}
		if d := e.mustAdmit(known, hello(known, a2a("hello"))); d.HirePrincipal != "node:"+known.kid {
			t.Errorf("hire principal without auth %q", d.HirePrincipal)
		}

		// A known key declaring recon with an enroll:* key.
		hr := hello(known, a2a("hello"), a2a("recon"))
		hr.Auth = all.Secret
		d, err := e.admit(known, hr, nil)
		if withScopes {
			if err != nil || len(d.Served) != 2 || d.HirePrincipal != all.Principal() {
				t.Errorf("enroll:* with the flag: %v %v", d, err)
			}
		} else {
			wantCode(t, "enroll:* without tunnel.enroll_with_api_key (hire only)", err, tunnel.CodeServiceDenied)
		}

		// An unknown key with an enroll:hello key.
		fresh := newTestKey(t)
		hf := hello(fresh, a2a("hello"))
		hf.Auth = enrollHello.Secret
		d, err = e.admit(fresh, hf, nil)
		if withScopes {
			if err != nil || len(d.Served) != 1 || d.Key.TenantID != e.team || d.HirePrincipal != enrollHello.Principal() {
				t.Fatalf("enroll:hello with the flag: %+v %v", d, err)
			}
			// hello already had a key (known): the enrollment is announced.
			if len(d.Enrolled) != 1 || d.Enrolled[0] != (Binding{KeyID: fresh.kid, TenantID: e.team, Tenant: "default", Service: "hello"}) {
				t.Errorf("enrolled %+v", d.Enrolled)
			}
			ev := e.verifyAudit()
			last := ev[len(ev)-1]
			if last["api_key"] != enrollHello.Principal() || last["services"].([]any)[0].(map[string]any)["via"] != viaAPIKey {
				t.Errorf("audit %v", last)
			}
			// enroll:hello does not cover recon: service_denied (it enrolls something, not everything).
			f2 := newTestKey(t)
			h2 := hello(f2, a2a("hello"), a2a("recon"))
			h2.Auth = enrollHello.Secret
			_, err = e.admit(f2, h2, nil)
			wantCode(t, "enroll:hello declaring recon too", err, tunnel.CodeServiceDenied)
			// An enroll: scope binds existing tunnel services only.
			f3 := newTestKey(t)
			h3 := hello(f3, a2a("hello"), a2a("missing"))
			h3.Auth = all.Secret
			_, err = e.admit(f3, h3, nil)
			wantCode(t, "enroll:* for a missing service", err, tunnel.CodeServiceDenied)
			// An API key of another tenant than the key's enrolls nothing for it.
			acme := e.tenant("acme")
			e.service(acme, "hello", "a2a", false)
			other, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: acme, Name: "acme", Scopes: []auth.Scope{auth.ScopeEnrollAll}})
			if err != nil {
				t.Fatal(err)
			}
			h4 := hello(known, a2a("hello"), a2a("recon"))
			if err := e.store.RevokeBinding(ctx, known.kid, "recon", "test"); err != nil {
				t.Fatal(err)
			}
			h4.Services = append(h4.Services, a2a("extra"))
			e.service(e.team, "extra", "a2a", false)
			h4.Auth = other.Secret
			_, err = e.admit(known, h4, nil)
			wantCode(t, "an API key of another tenant", err, tunnel.CodeEnrollInvalid)
		} else {
			// SPEC-CONFLICT (lost welcome): an unknown key that nothing enrolls is unknown_key, not service_denied.
			wantCode(t, "an unknown key with a scope the gate does not honour", err, tunnel.CodeUnknownKey)
		}
		// A gate that takes no API keys refuses any auth.
		noKeys := NewStore(e.pool, Options{PublicURL: "https://gate.example"})
		hn := hello(known, a2a("hello"))
		hn.Auth = hire.Secret
		_, err = noKeys.Admit(ctx, Request{Hello: hn, PubKey: known.pub})
		wantCode(t, "auth at a gate without API keys", err, tunnel.CodeAuthInvalid)
		e.verifyAudit()
	}
}

// Rotation (§12): the new key in the old key's tenant with the old key's bindings (a revoked one stays
// revoked, TestAdmitRotationKeepsRevokedBindings), the old key
// rotated with a 24 h overlap, under a row lock: of two concurrent rotations one wins. A rotated
// rotate_from is enroll_invalid, a revoked one revoked; the rotated key itself is revoked; the new key
// reconnects alone (the node's answer to a lost welcome).
func TestAdmitRotation(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	enroll := func(k *testKey, services ...string) {
		var decl []tunnel.Service
		var toks []string
		for _, s := range services {
			decl = append(decl, a2a(s))
			toks = append(toks, e.token(e.team, s))
		}
		h := hello(k, decl...)
		h.Enroll = toks
		e.mustAdmit(k, h)
	}
	old := newTestKey(t)
	enroll(old, "hello", "recon")
	if err := e.store.RevokeBinding(ctx, old.kid, "recon", "test"); err != nil {
		t.Fatal(err)
	}

	nk := newTestKey(t)
	d, err := e.admit(nk, hello(nk, a2a("hello"), a2a("recon")), old)
	if err != nil {
		t.Fatal(err)
	}
	if got := served(d); !slices.Equal(got, []string{"a2a:hello@https://gate.example/agents/hello"}) {
		t.Errorf("served %v (recon's binding was revoked)", got)
	}
	if d.Rotated == nil || d.Rotated.KeyID != old.kid || d.Rotated.RotatedTo != nk.kid || d.Rotated.RevokedReason != ReasonRotated ||
		d.Key.TenantID != e.team || d.Key.KeyID != nk.kid {
		t.Fatalf("decision %+v rotated %+v", d, d.Rotated)
	}
	var (
		reason, rotatedTo string
		revokedAt         *time.Time
		overlap           time.Time
		inOverlap         bool
	)
	err = e.pool.QueryRow(ctx, `select revoked_reason, rotated_to, revoked_at, overlap_until,
		overlap_until between now() + interval '24 hours' - interval '1 minute' and now() + interval '24 hours'
		from kontra.tunnel_keys where key_id = $1`, old.kid).Scan(&reason, &rotatedTo, &revokedAt, &overlap, &inOverlap)
	if err != nil {
		t.Fatal(err)
	}
	if reason != ReasonRotated || rotatedTo != nk.kid || revokedAt != nil || !inOverlap || !overlap.Equal(*d.Rotated.OverlapUntil) {
		t.Errorf("old key: reason %s rotated_to %s revoked_at %v overlap %s", reason, rotatedTo, revokedAt, overlap)
	}
	var newServices []string
	rows, _ := e.pool.Query(ctx, `select service from kontra.key_services where key_id = $1 and revoked_at is null order by service`, nk.kid)
	newServices, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(newServices, []string{"hello"}) {
		t.Errorf("new key's active bindings %v %v (recon's revoked row is carried over revoked)", newServices, err)
	}
	ev := e.verifyAudit()
	last := ev[len(ev)-1]
	if last["event"] != EventRotated || last["old_key"] != old.kid || last["new_key"] != nk.kid {
		t.Errorf("audit %v", last)
	}

	// A rotate_from naming the rotated key: enroll_invalid (the lost-welcome case the node avoids).
	n2 := newTestKey(t)
	_, err = e.admit(n2, hello(n2, a2a("hello")), old)
	wantCode(t, "rotate_from naming a rotated key", err, tunnel.CodeEnrollInvalid)
	// The same rotation sent again: the new key exists.
	_, err = e.admit(nk, hello(nk, a2a("hello")), old)
	wantCode(t, "a rotation whose new key exists", err, tunnel.CodeEnrollInvalid)
	// The rotated key connecting as itself: revoked.
	_, err = e.admit(old, hello(old, a2a("hello")), nil)
	wantCode(t, "the rotated key itself", err, tunnel.CodeRevoked)
	// The new key alone (what the node sends after a lost welcome): welcome.
	if d := e.mustAdmit(nk, hello(nk, a2a("hello"))); len(d.Served) != 1 {
		t.Errorf("the new key reconnecting: served %v", served(d))
	}
	// A rotation that did not commit leaves the new key unknown: unknown_key, which the node retries.
	n3 := newTestKey(t)
	_, err = e.admit(n3, hello(n3, a2a("hello")), nil)
	wantCode(t, "an uncommitted rotation's new key alone", err, tunnel.CodeUnknownKey)
	// rotate_from naming a revoked key, an unknown key, the active key.
	revoked := newTestKey(t)
	enroll(revoked, "hello")
	if err := e.store.RevokeKey(ctx, revoked.kid, ReasonOwner, "test"); err != nil {
		t.Fatal(err)
	}
	_, err = e.admit(n3, hello(n3, a2a("hello")), revoked)
	wantCode(t, "rotate_from naming a revoked key", err, tunnel.CodeRevoked)
	_, err = e.admit(n3, hello(n3, a2a("hello")), newTestKey(t))
	wantCode(t, "rotate_from naming no key of this gate", err, tunnel.CodeEnrollInvalid)
	_, err = e.admit(nk, hello(nk, a2a("hello")), nk)
	wantCode(t, "rotate_from naming the active key", err, tunnel.CodeEnrollInvalid)
	// rotate_from whose stored public key is another one: bad_proof.
	impostor := newTestKey(t)
	e.exec(`insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ($1, $2, $3, 'x')`,
		impostor.kid, e.team, newTestKey(t).pub)
	_, err = e.admit(n3, hello(n3, a2a("hello")), impostor)
	wantCode(t, "rotate_from stored with another public key", err, tunnel.CodeBadProof)

	// Two concurrent rotations of one key to two new keys: exactly one wins.
	base := newTestKey(t)
	enroll(base, "hello")
	successors := []*testKey{newTestKey(t), newTestKey(t)}
	results := e.contend(2, `select 1 from kontra.tunnel_keys where key_id = $1 for update`, []any{base.kid}, func(i int) error {
		_, err := e.admit(successors[i], hello(successors[i], a2a("hello")), base)
		return err
	})
	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
			continue
		}
		wantCode(t, "the losing rotation", err, tunnel.CodeEnrollInvalid)
	}
	if wins != 1 {
		t.Errorf("%d rotations won, want 1: %v", wins, results)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where rotated_to is not null and key_id = $1`, base.kid); n != 1 {
		t.Error("the base key is not rotated once")
	}
	e.verifyAudit()
}

// Recipients are the keys enrolled for the service, neither revoked nor rotated, connected or not (D61).
func TestRecipients(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	enroll := func(services ...string) *testKey {
		k := newTestKey(t)
		var decl []tunnel.Service
		var toks []string
		for _, s := range services {
			decl = append(decl, a2a(s))
			toks = append(toks, e.token(e.team, s))
		}
		h := hello(k, decl...)
		h.Enroll = toks
		e.mustAdmit(k, h)
		return k
	}
	active := enroll("hello")
	revokedKey := enroll("hello")
	revokedBinding := enroll("hello", "recon")
	rotated := enroll("hello")
	other := enroll("recon")
	if err := e.store.RevokeKey(ctx, revokedKey.kid, ReasonOperator, "t"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RevokeBinding(ctx, revokedBinding.kid, "hello", "t"); err != nil {
		t.Fatal(err)
	}
	successor := newTestKey(t)
	if _, err := e.admit(successor, hello(successor, a2a("hello")), rotated); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.Recipients(ctx, e.team, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{active.recipient, successor.recipient}) {
		t.Errorf("recipients %v, want the active key and the rotation's successor", got)
	}
	got, err = e.store.Recipients(ctx, e.team, "recon")
	if err != nil || !slices.Equal(got, []string{revokedBinding.recipient, other.recipient}) {
		t.Errorf("recon recipients %v %v", got, err)
	}
	if got, err := e.store.Recipients(ctx, e.team, "nope"); err != nil || len(got) != 0 {
		t.Errorf("an unknown service: %v %v", got, err)
	}
}

// A revoked binding stays left out whatever standing authority the hello carries (§5): the auth API key
// rides on every hello as the hiring principal, so even with enroll:* (or enroll:<svc>) and
// tunnel.enroll_with_api_key it never enrolls the binding again. Only a one-time token in the hello does
// (SPEC-CONFLICT, admit.go).
func TestAdmitAPIKeyKeepsRevokedBinding(t *testing.T) {
	e := newPGEnv(t, func(o *Options) { o.EnrollWithAPIKey = true })
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	all, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: e.team, Name: "node", Scopes: []auth.Scope{auth.ScopeHire, auth.ScopeEnrollAll}})
	if err != nil {
		t.Fatal(err)
	}
	reconOnly, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: e.team, Name: "recon", Scopes: []auth.Scope{auth.EnrollScope("recon")}})
	if err != nil {
		t.Fatal(err)
	}
	k := newTestKey(t)
	h := hello(k, a2a("hello"), a2a("recon"))
	h.Auth = all.Secret
	if d := e.mustAdmit(k, h); len(d.Served) != 2 {
		t.Fatalf("enrolled by enroll:*: served %v", served(d))
	}
	if err := e.store.RevokeBinding(ctx, k.kid, "recon", "test"); err != nil {
		t.Fatal(err)
	}
	enrolledRows := func() int {
		n := 0
		for _, ev := range events(e.verifyAudit()) {
			if ev == EventEnrolled {
				n++
			}
		}
		return n
	}
	before := enrolledRows()
	helloOnly := []string{"a2a:hello@https://gate.example/agents/hello"}
	for _, key := range []auth.Minted{all, reconOnly} {
		h := hello(k, a2a("hello"), a2a("recon"))
		h.Auth = key.Secret
		d := e.mustAdmit(k, h)
		if got := served(d); !slices.Equal(got, helloOnly) {
			t.Errorf("auth %v: served %v, want hello alone (recon's binding was revoked)", key.Scopes, got)
		}
		if d.HirePrincipal != key.Principal() {
			t.Errorf("hire principal %q, want %q", d.HirePrincipal, key.Principal())
		}
	}
	if n := e.count(`select count(*) from kontra.key_services where key_id = $1 and service = 'recon' and revoked_at is not null`, k.kid); n != 1 {
		t.Error("the auth API key cleared recon's revocation")
	}
	if n := enrolledRows(); n != before {
		t.Errorf("%d enrollment audit rows appended by handshakes that enroll nothing", n-before)
	}
	// A one-time service token in the hello is the operator's explicit re-enrollment.
	h2 := hello(k, a2a("hello"), a2a("recon"))
	h2.Auth = all.Secret
	h2.Enroll = []string{e.token(e.team, "recon")}
	if d := e.mustAdmit(k, h2); len(d.Served) != 2 {
		t.Errorf("a service token re-enrolling recon: served %v", served(d))
	}
	ev := e.verifyAudit()
	last := ev[len(ev)-1]
	if svcs := last["services"].([]any); len(svcs) != 1 || svcs[0].(map[string]any)["via"] != viaServiceToken {
		t.Errorf("re-enrollment audit %v", last)
	}
}

// A rotation carries a revoked binding over as revoked (SPEC-CONFLICT, rotate.go): the new key's later
// handshakes declaring the same services leave it out as the old key's did, rather than refusing the whole
// handshake with service_denied (the node's redial after a lost welcome, a restart with the same --serve).
// The carried row grants nothing; a fresh token enrolls the new key for it again.
func TestAdmitRotationKeepsRevokedBindings(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	old := e.enrolled("hello", "recon")
	if err := e.store.RevokeBinding(ctx, old.kid, "recon", "test"); err != nil {
		t.Fatal(err)
	}
	var revokedAt time.Time
	if err := e.pool.QueryRow(ctx, `select revoked_at from kontra.key_services where key_id = $1 and service = 'recon'`, old.kid).
		Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	helloOnly := []string{"a2a:hello@https://gate.example/agents/hello"}
	if got := served(e.mustAdmit(old, hello(old, a2a("hello"), a2a("recon")))); !slices.Equal(got, helloOnly) {
		t.Fatalf("the old key: served %v", got)
	}
	nk := newTestKey(t)
	d, err := e.admit(nk, hello(nk, a2a("hello"), a2a("recon")), old)
	if err != nil || !slices.Equal(served(d), helloOnly) {
		t.Fatalf("the rotation: %v %v", d, err)
	}
	// The redial with the new key alone and the same services.
	d, err = e.admit(nk, hello(nk, a2a("hello"), a2a("recon")), nil)
	if err != nil {
		t.Fatalf("the new key declaring the services it declared before the rotation: %v", err)
	}
	if got := served(d); !slices.Equal(got, helloOnly) {
		t.Errorf("the new key: served %v", got)
	}
	var carried *time.Time
	if err := e.pool.QueryRow(ctx, `select revoked_at from kontra.key_services where key_id = $1 and service = 'recon'`, nk.kid).
		Scan(&carried); err != nil {
		t.Fatalf("the new key has no recon row: %v", err)
	}
	if carried == nil || !carried.Equal(revokedAt) {
		t.Errorf("the new key's recon row: revoked_at %v, want the old row's %s", carried, revokedAt)
	}
	if got, err := e.store.Recipients(ctx, e.team, "recon"); err != nil || len(got) != 0 {
		t.Errorf("recon's recipients %v %v: a carried revoked row grants nothing", got, err)
	}
	ev := e.verifyAudit()
	var rotated map[string]any
	for _, r := range ev {
		if r["event"] == EventRotated {
			rotated = r
		}
	}
	if s, ok := rotated["services"].([]any); !ok || len(s) != 1 || s[0] != "hello" {
		t.Errorf("rotation audit services %v", rotated["services"])
	}
	if s, ok := rotated["revoked_services"].([]any); !ok || len(s) != 1 || s[0] != "recon" {
		t.Errorf("rotation audit revoked_services %v", rotated["revoked_services"])
	}
	// A fresh token enrolls the new key for recon again.
	h := hello(nk, a2a("hello"), a2a("recon"))
	h.Enroll = []string{e.token(e.team, "recon")}
	if d := e.mustAdmit(nk, h); len(d.Served) != 2 {
		t.Errorf("re-enrolling recon for the new key: served %v", served(d))
	}
	e.verifyAudit()
}

// Admit announces each binding it adds to a service that already had a key with NOTIFY kontra_tunnel
// 'enrolled:<key_id>/<tenant_id>/<service>' in its own transaction (§5; M2 review of WP-04): the notice
// rides the binding's commit, so it reaches every replica whatever becomes of the new key's session. The
// service's first key, a reconnect, and a refused admission announce nothing.
func TestAdmitAnnouncesEnrollments(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.service(e.team, "hello", "a2a", false)
	e.service(e.team, "recon", "a2a", false)
	listener, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) })
	if _, err := listener.Exec(ctx, `listen `+string(pg.ChannelTunnel)); err != nil {
		t.Fatal(err)
	}
	next := func(within time.Duration) (string, bool) {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, within)
		defer cancel()
		n, err := listener.WaitForNotification(wctx)
		if err != nil {
			if wctx.Err() == nil {
				t.Fatal(err)
			}
			return "", false
		}
		return n.Payload, true
	}

	first := newTestKey(t)
	h := hello(first, a2a("hello"))
	h.Enroll = []string{e.token(e.team, "hello")}
	e.mustAdmit(first, h)
	if p, ok := next(300 * time.Millisecond); ok {
		t.Fatalf("the service's first key announced %q", p)
	}

	second := newTestKey(t)
	h2 := hello(second, a2a("hello"), a2a("recon"))
	h2.Enroll = []string{e.token(e.team, "hello"), e.token(e.team, "recon")}
	d := e.mustAdmit(second, h2)
	want := Binding{KeyID: second.kid, TenantID: e.team, Service: "hello"}
	if len(d.Enrolled) != 1 || d.Enrolled[0].Service != "hello" {
		t.Fatalf("enrolled %+v", d.Enrolled)
	}
	p, ok := next(5 * time.Second)
	if !ok || p != EnrolledPayload(want) {
		t.Fatalf("NOTIFY kontra_tunnel %q (%v), want %q", p, ok, EnrolledPayload(want))
	}
	if got, ok := ParseEnrolledPayload(p); !ok || got != want {
		t.Fatalf("ParseEnrolledPayload(%q) = %+v, %v", p, got, ok)
	}
	if p, ok := next(300 * time.Millisecond); ok {
		t.Fatalf("a second announcement %q (recon had no other key)", p)
	}

	// A reconnect enrolls nothing; a refused admission (a used token) commits nothing.
	e.mustAdmit(second, hello(second, a2a("hello")))
	third := newTestKey(t)
	h3 := hello(third, a2a("hello"))
	h3.Enroll = h2.Enroll[:1]
	_, err = e.admit(third, h3, nil)
	wantCode(t, "a used token", err, tunnel.CodeEnrollInvalid)
	if p, ok := next(300 * time.Millisecond); ok {
		t.Fatalf("announced %q without an enrollment", p)
	}

	for _, bad := range []string{"enrolled:", "enrolled:" + second.kid + "/" + e.team.String(), "enrolled:x/" + e.team.String() + "/hello",
		"enrolled:" + second.kid + "/nope/hello", "enrolled:" + second.kid + "/" + e.team.String() + "/Hello", "session:" + e.team.String()} {
		if b, ok := ParseEnrolledPayload(bad); ok {
			t.Errorf("ParseEnrolledPayload(%q) = %+v", bad, b)
		}
	}
}

// A node that only hires (spec/tunnel.md §4 "a node that only hires", §9; the M3 plan's conflict on §4–§5,
// WP-03): an unknown key whose hello declares no service and carries an API key with the hire scope is admitted
// with a tunnel_keys row in the API key's tenant and no binding, its hiring principal the API key's; without such
// a key it stays unknown_key (D133), and so does one declaring services nothing enrolls. The key reconnects
// without auth as a known key, hiring as node:<key_id>.
func TestAdmitHireOnly(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	acme := e.tenant("acme")
	e.service(acme, "hello", "a2a", false)
	mint := func(tenantID uuid.UUID, scopes ...auth.Scope) auth.Minted {
		m, err := e.keys.Mint(ctx, auth.MintRequest{TenantID: tenantID, Name: "hirer", Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	hire := mint(acme, auth.ScopeHire)
	enrollOnly := mint(acme, auth.ScopeEnrollAll)

	refused := map[string]func(k *testKey) *tunnel.Hello{
		"no auth": func(k *testKey) *tunnel.Hello { return hello(k) },
		"an API key without the hire scope": func(k *testKey) *tunnel.Hello {
			h := hello(k)
			h.Auth = enrollOnly.Secret
			return h
		},
		"declaring a service nothing enrolls": func(k *testKey) *tunnel.Hello {
			h := hello(k, a2a("hello"))
			h.Auth = hire.Secret
			return h
		},
	}
	for name, mk := range refused {
		k := newTestKey(t)
		_, err := e.admit(k, mk(k), nil)
		wantCode(t, name, err, tunnel.CodeUnknownKey)
		if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1`, k.kid); n != 0 {
			t.Errorf("%s: the key was written", name)
		}
	}

	k := newTestKey(t)
	h := hello(k)
	h.Auth = hire.Secret
	d := e.mustAdmit(k, h)
	if len(d.Served) != 0 || d.Key.TenantID != acme || d.Key.Tenant != "acme" || d.HirePrincipal != hire.Principal() {
		t.Fatalf("hire-only decision %+v", d)
	}
	if d.Hire == nil || d.Hire.ID != hire.Principal() || d.Hire.KeyID == nil || *d.Hire.KeyID != hire.ID || d.Hire.BuyerID != hire.BuyerID ||
		!d.Hire.MayHire() {
		t.Fatalf("the decision's hiring principal %+v", d.Hire)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2 and revoked_at is null`, k.kid, acme); n != 1 {
		t.Fatal("no tunnel_keys row in the API key's tenant")
	}
	if n := e.count(`select count(*) from kontra.key_services where key_id = $1`, k.kid); n != 0 {
		t.Fatalf("a hire-only key has %d bindings", n)
	}
	ev := e.verifyAudit()
	last := ev[len(ev)-1]
	if last["event"] != EventEnrolled || last["hire_only"] != true || last["api_key"] != hire.Principal() || last["key_id"] != k.kid ||
		len(last["services"].([]any)) != 0 {
		t.Fatalf("audit %v", last)
	}

	// The same key again, without auth: a known key that hires as itself.
	again := e.mustAdmit(k, hello(k))
	if again.HirePrincipal != "node:"+k.kid || again.Hire != nil || len(again.Served) != 0 || again.Key.TenantID != acme {
		t.Fatalf("the hire-only key without auth: %+v", again)
	}
	// And with auth again: the API key's principal, no second row.
	h2 := hello(k)
	h2.Auth = hire.Secret
	if d := e.mustAdmit(k, h2); d.HirePrincipal != hire.Principal() || d.Hire == nil {
		t.Fatalf("the hire-only key with auth: %+v", d)
	}
	if n := e.count(`select count(*) from kontra.tunnel_keys where tenant_id = $1`, acme); n != 1 {
		t.Fatalf("%d keys in acme", n)
	}
}
