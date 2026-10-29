package enroll

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/tunnel"
)

// convertEnv is a multi-tenant gate with tunnel.anonymous and tunnel.enroll_with_api_key, an account "acme" and a key
// of it with hire and enroll:* (what kontra login stores, spec/tunnel.md §6.3).
type convertEnv struct {
	*pgEnv
	account uuid.UUID
	key     auth.Minted
}

func newConvertEnv(t *testing.T, mod ...func(*Options)) *convertEnv {
	t.Helper()
	lim := anon.Defaults
	e := newPGEnv(t, append([]func(*Options){func(o *Options) { o.Multi = true; o.Anonymous = &lim; o.EnrollWithAPIKey = true }}, mod...)...)
	c := &convertEnv{pgEnv: e, account: e.tenant("acme")}
	var err error
	c.key, err = e.keys.Mint(context.Background(), auth.MintRequest{TenantID: c.account, Name: "login",
		Scopes: []auth.Scope{auth.ScopeHire, auth.ScopeEnrollAll}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// anonymous opens an anonymous session for k declaring services (hello public).
func (c *convertEnv) anonymous(k *testKey, services ...tunnel.Service) *Decision {
	c.t.Helper()
	h := anonHello(k, services...)
	h.Public = []string{"hello"}
	d, err := c.admitFrom(k, h, "203.0.113.9")
	if err != nil || d.Anonymous == nil {
		c.t.Fatalf("the anonymous session: %+v %v", d, err)
	}
	return d
}

// withAuth is the hello kontra connect sends after kontra login: no anonymous field, the API key as auth.
func withAuth(k *testKey, key string, services ...tunnel.Service) *tunnel.Hello {
	h := hello(k, services...)
	h.Auth = key
	return h
}

// spec/tunnel.md §6.3 (D278): a hello whose key belongs to a live anonymous tenant and whose auth is an account's API
// key with enroll:* converts the key in the admission's transaction: the declared services the session serves are
// created in the account where it has none (same names, allow [org:<account>]; an existing one is bound, not
// recreated), the key moves to the account with bindings to them (its anonymous bindings gone), the anonymous tenant
// ends at once (expires_at now, services disabled), the decision names the account's URLs with no anonymous part and
// the API key as the hiring principal, and an audit row records the conversion; the next hello is §5's, with no
// conversion.
func TestAdmitConvertsWithAnAccountKey(t *testing.T) {
	c := newConvertEnv(t)
	ctx := context.Background()
	c.service(c.account, "existing", "a2a", false)
	k := newTestKey(t)
	d1 := c.anonymous(k, a2a("hello"), mcp("files"), a2a("existing"))
	from := d1.Key.TenantID

	d, err := c.admitFrom(k, withAuth(k, c.key.Secret, a2a("hello"), mcp("files"), a2a("existing")), "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if d.Anonymous != nil || d.Key.TenantID != c.account || d.Key.Tenant != "acme" || d.HirePrincipal != c.key.Principal() ||
		d.Hire == nil || d.Hire.ID != c.key.Principal() {
		t.Fatalf("the conversion's decision %+v %+v", d.Key, d.Anonymous)
	}
	if got := served(d); !slices.Equal(got, []string{"a2a:hello@https://gate.example/agents/acme/hello",
		"mcp:files@https://gate.example/mcp/acme/files", "a2a:existing@https://gate.example/agents/acme/existing"}) {
		t.Fatalf("served %v", got)
	}
	for name, want := range map[string]string{"hello": "a2a", "files": "mcp"} {
		var (
			kind, source string
			allow        []string
			tunnelled    bool
		)
		if err := c.pool.QueryRow(ctx, `select kind, source, allow, backend -> 'tunnel' is not null from kontra.services
			where tenant_id = $1 and name = $2 and disabled_at is null`, c.account, name).Scan(&kind, &source, &allow, &tunnelled); err != nil ||
			kind != want || source != "db" || !slices.Equal(allow, []string{"org:acme"}) || !tunnelled {
			t.Errorf("the account's %s: %s %s %v %v %v", name, kind, source, allow, tunnelled, err)
		}
	}
	if n := c.count(`select count(*) from kontra.services where tenant_id = $1 and name = 'existing' and source = 'config'`, c.account); n != 1 {
		t.Error("the existing service was recreated")
	}
	if n := c.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2 and revoked_at is null`, k.kid, c.account); n != 1 {
		t.Fatal("the key did not move to the account")
	}
	if n := c.count(`select count(*) from kontra.key_services where key_id = $1 and tenant_id = $2 and revoked_at is null`, k.kid, c.account); n != 3 {
		t.Errorf("%d bindings in the account, want 3", n)
	}
	if n := c.count(`select count(*) from kontra.key_services where tenant_id = $1`, from); n != 0 {
		t.Errorf("%d bindings left in the anonymous tenant", n)
	}
	if n := c.count(`select count(*) from kontra.tenants where id = $1 and expires_at <= now() and kind = 'anonymous'`, from); n != 1 {
		t.Error("the anonymous tenant did not end at once")
	}
	if n := c.count(`select count(*) from kontra.services where tenant_id = $1 and disabled_at is null`, from); n != 0 {
		t.Errorf("%d anonymous services still enabled", n)
	}
	if n := c.count(`select count(*) from kontra.tenants where id = $1 and expires_at is null and deleted_at is null`, c.account); n != 1 {
		t.Error("the account expires")
	}

	var converted, ended, created int
	for _, r := range c.verifyAudit() {
		switch r["event"] {
		case anon.EventConverted:
			converted++
			if r["key_id"] != k.kid || r["from"] != d1.Key.Tenant || r["from_id"] != from.String() || r["to"] != "acme" ||
				r["to_id"] != c.account.String() || r["via"] != anon.ViaAPIKey || r["api_key"] != c.key.Principal() ||
				strings.Join(anyStrs(r["services"]), ",") != "existing,files,hello" {
				t.Errorf("the conversion's audit row %v", r)
			}
		case anon.EventExpired:
			if r["tenant_id"] == from.String() && r["reason"] == "converted" {
				ended++
			}
		case EventServiceCreated:
			if r["tenant"] == "acme" {
				created++
			}
		}
	}
	if converted != 1 || ended != 1 || created != 2 {
		t.Errorf("audit: %d conversions, %d ends, %d services created", converted, ended, created)
	}

	// The next hello is the account key's (§5): no conversion, no anonymous part, the same URLs.
	d2, err := c.admitFrom(k, withAuth(k, c.key.Secret, a2a("hello"), mcp("files"), a2a("existing")), "203.0.113.9")
	if err != nil || d2.Anonymous != nil || d2.Key.TenantID != c.account || len(d2.Served) != 3 {
		t.Fatalf("the next hello: %+v %v", d2, err)
	}
	n := 0
	for _, r := range c.verifyAudit() {
		if r["event"] == anon.EventConverted {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d conversions after the next hello", n)
	}
}

func anyStrs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// §6.3: an existing account service of another kind is kind_mismatch, and nothing moves: the key stays in its live
// anonymous tenant, which keeps its services, the account gets no service, and nothing is audited.
func TestAdmitConversionKindMismatchMovesNothing(t *testing.T) {
	c := newConvertEnv(t)
	c.service(c.account, "hello", "mcp", false)
	k := newTestKey(t)
	d1 := c.anonymous(k, a2a("hello"), a2a("other"))
	before := len(c.verifyAudit())
	_, err := c.admitFrom(k, withAuth(k, c.key.Secret, a2a("hello"), a2a("other")), "203.0.113.9")
	wantCode(t, "a declared a2a service the account has as mcp", err, tunnel.CodeKindMismatch)
	if n := c.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2`, k.kid, d1.Key.TenantID); n != 1 {
		t.Fatal("the key moved")
	}
	if n := c.count(`select count(*) from kontra.tenants where id = $1 and expires_at > now()`, d1.Key.TenantID); n != 1 {
		t.Fatal("the anonymous tenant ended")
	}
	if n := c.count(`select count(*) from kontra.key_services where key_id = $1 and tenant_id = $2 and revoked_at is null`, k.kid, d1.Key.TenantID); n != 2 {
		t.Errorf("%d anonymous bindings left, want 2", n)
	}
	if n := c.count(`select count(*) from kontra.services where tenant_id = $1`, c.account); n != 1 {
		t.Errorf("%d account services, want only the existing one", n)
	}
	if after := len(c.verifyAudit()); after != before {
		t.Errorf("%d audit rows written by a refused conversion", after-before)
	}
}

// What does not convert (§6.3, D286): a hire-only key, an enroll: scope for some services only, and an enroll:* key
// on a gate without tunnel.enroll_with_api_key resume the anonymous session with the key as its hiring principal;
// what does: a hello declaring no service (admitted hire-only in the account), the team tenant's enroll:* key, and an
// enroll:* key on a gate that stopped opening anonymous sessions.
func TestAdmitConversionCases(t *testing.T) {
	ctx := context.Background()
	resumed := func(t *testing.T, c *convertEnv, scopes []auth.Scope, mod ...func(*Options)) {
		t.Helper()
		key, err := c.keys.Mint(ctx, auth.MintRequest{TenantID: c.account, Name: "k", Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		k := newTestKey(t)
		d1 := c.anonymous(k, a2a("hello"))
		d, err := c.admitFrom(k, withAuth(k, key.Secret, a2a("hello")), "203.0.113.9")
		if err != nil || d.Anonymous == nil || d.Key.TenantID != d1.Key.TenantID || d.HirePrincipal != key.Principal() {
			t.Fatalf("%v: %+v %v", scopes, d, err)
		}
		if n := c.count(`select count(*) from kontra.services where tenant_id = $1`, c.account); n != 0 {
			t.Errorf("%v: created %d account services", scopes, n)
		}
	}
	t.Run("hire only", func(t *testing.T) { resumed(t, newConvertEnv(t), []auth.Scope{auth.ScopeHire}) })
	t.Run("enroll:hello", func(t *testing.T) {
		resumed(t, newConvertEnv(t), []auth.Scope{auth.ScopeHire, auth.EnrollScope("hello")})
	})
	t.Run("without enroll_with_api_key", func(t *testing.T) {
		resumed(t, newConvertEnv(t, func(o *Options) { o.EnrollWithAPIKey = false }), []auth.Scope{auth.ScopeHire, auth.ScopeEnrollAll})
	})
	t.Run("no services", func(t *testing.T) {
		c := newConvertEnv(t)
		k := newTestKey(t)
		d1 := c.anonymous(k, a2a("hello"))
		d, err := c.admitFrom(k, withAuth(k, c.key.Secret), "203.0.113.9")
		if err != nil || d.Anonymous != nil || d.Key.TenantID != c.account || len(d.Served) != 0 || d.HirePrincipal != c.key.Principal() {
			t.Fatalf("%+v %v", d, err)
		}
		if n := c.count(`select count(*) from kontra.tenants where id = $1 and expires_at <= now()`, d1.Key.TenantID); n != 1 {
			t.Error("the anonymous tenant did not end")
		}
	})
	t.Run("the team's key", func(t *testing.T) {
		c := newConvertEnv(t)
		key, err := c.keys.Mint(ctx, auth.MintRequest{TenantID: c.team, Name: "team", Scopes: []auth.Scope{auth.ScopeHire, auth.ScopeEnrollAll}})
		if err != nil {
			t.Fatal(err)
		}
		k := newTestKey(t)
		c.anonymous(k, a2a("hello"))
		d, err := c.admitFrom(k, withAuth(k, key.Secret, a2a("hello")), "203.0.113.9")
		if err != nil || d.Anonymous != nil || d.Key.Tenant != "default" ||
			!slices.Equal(served(d), []string{"a2a:hello@https://gate.example/agents/default/hello"}) {
			t.Fatalf("%+v %v", d, err)
		}
	})
	// The M4 WP-06 review: a conversion creates only what the anonymous session serves (§6.1 "the services keep their
	// names", D296). Any other declared name is §5's: an account service that exists is bound, one that does not is
	// service_denied "no such service", as for the same key on §5's own path, and nothing moves. An anonymous session
	// costs no credential, so otherwise any enroll:* key could create services (M2's C10: only tenant tokens and the
	// operator do), past §6.2's services limit too.
	refusedConversion := func(t *testing.T, c *convertEnv, k *testKey, from uuid.UUID, h *tunnel.Hello, names ...string) {
		t.Helper()
		before := len(c.verifyAudit())
		_, err := c.admitFrom(k, h, "203.0.113.9")
		wantCode(t, "a conversion declaring services the session does not serve", err, tunnel.CodeServiceDenied)
		for _, n := range names {
			if !strings.Contains(err.Error(), n+": no such service") {
				t.Errorf("the refusal does not name %s: %v", n, err)
			}
		}
		if n := c.count(`select count(*) from kontra.tunnel_keys where key_id = $1 and tenant_id = $2`, k.kid, from); n != 1 {
			t.Fatal("the key moved")
		}
		if n := c.count(`select count(*) from kontra.tenants where id = $1 and expires_at > now()`, from); n != 1 {
			t.Fatal("the anonymous tenant ended")
		}
		if n := c.count(`select count(*) from kontra.services where tenant_id = $1`, c.account); n != 0 {
			t.Errorf("%d account services created", n)
		}
		if after := len(c.verifyAudit()); after != before {
			t.Errorf("%d audit rows written by a refused conversion", after-before)
		}
	}
	t.Run("names the session does not serve", func(t *testing.T) {
		c := newConvertEnv(t)
		k := newTestKey(t)
		d1 := c.anonymous(k, a2a("hello"))
		refusedConversion(t, c, k, d1.Key.TenantID, withAuth(k, c.key.Secret, a2a("hello"), a2a("payroll"), mcp("admin")), "payroll", "admin")
		// More services than an anonymous session may serve (§6.2's services, 4) are refused the same way.
		many := []tunnel.Service{a2a("hello")}
		for i := range anon.Defaults.Services + 1 {
			many = append(many, a2a(fmt.Sprintf("extra%d", i)))
		}
		refusedConversion(t, c, k, d1.Key.TenantID, withAuth(k, c.key.Secret, many...), "extra0", "extra4")
	})
	t.Run("another kind than the session's", func(t *testing.T) {
		c := newConvertEnv(t)
		k := newTestKey(t)
		d1 := c.anonymous(k, a2a("hello"))
		refusedConversion(t, c, k, d1.Key.TenantID, withAuth(k, c.key.Secret, mcp("hello")), "hello")
	})
	t.Run("a service the session no longer serves", func(t *testing.T) {
		c := newConvertEnv(t)
		k := newTestKey(t)
		c.anonymous(k, a2a("hello"), a2a("old"))
		d1 := c.anonymous(k, a2a("hello")) // resumed: old disabled and unbound, its row kept until the tenant ends
		refusedConversion(t, c, k, d1.Key.TenantID, withAuth(k, c.key.Secret, a2a("hello"), a2a("old")), "old")
	})
	t.Run("an account service the session does not serve is bound", func(t *testing.T) {
		c := newConvertEnv(t)
		c.service(c.account, "ledger", "a2a", false)
		k := newTestKey(t)
		c.anonymous(k, a2a("hello"))
		d, err := c.admitFrom(k, withAuth(k, c.key.Secret, a2a("hello"), a2a("ledger")), "203.0.113.9")
		if err != nil || d.Anonymous != nil || !slices.Equal(served(d), []string{"a2a:hello@https://gate.example/agents/acme/hello",
			"a2a:ledger@https://gate.example/agents/acme/ledger"}) {
			t.Fatalf("%+v %v", d, err)
		}
		if n := c.count(`select count(*) from kontra.services where tenant_id = $1`, c.account); n != 2 {
			t.Errorf("%d account services, want ledger and the created hello", n)
		}
	})
	t.Run("a gate that stopped opening anonymous sessions", func(t *testing.T) {
		c := newConvertEnv(t)
		k := newTestKey(t)
		d1 := c.anonymous(k, a2a("hello"))
		off := NewStore(c.pool, Options{PublicURL: "https://gate.example", Keys: c.keys, Multi: true, EnrollWithAPIKey: true})
		h := withAuth(k, c.key.Secret, a2a("hello"))
		d, err := off.Admit(ctx, Request{Hello: h, PubKey: k.pub, SessionID: uuid.Must(uuid.NewV7()), ReplicaID: c.replic})
		if err != nil || d.Anonymous != nil || d.Key.TenantID != c.account || len(d.Served) != 1 {
			t.Fatalf("%+v %v", d, err)
		}
		if n := c.count(`select count(*) from kontra.tenants where id = $1 and expires_at <= now()`, d1.Key.TenantID); n != 1 {
			t.Error("the anonymous tenant did not end")
		}
	})
}

// anon.Convert refuses what is not a conversion: a key that has not moved, and a tenant that is not anonymous.
func TestConvertChecksTheMove(t *testing.T) {
	c := newConvertEnv(t)
	ctx := context.Background()
	k := newTestKey(t)
	d1 := c.anonymous(k, a2a("hello"))
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := anon.Convert(ctx, tx, anon.ConvertRequest{KeyID: k.kid, From: d1.Key.TenantID, To: c.account}); err == nil {
		t.Error("a key still in its anonymous tenant was converted")
	}
	if err := anon.MoveKey(ctx, tx, k.kid, c.account); err != nil {
		t.Fatal(err)
	}
	if err := anon.Convert(ctx, tx, anon.ConvertRequest{KeyID: k.kid, From: c.team, To: c.account}); err == nil {
		t.Error("a team tenant was ended as an anonymous one")
	}
}
