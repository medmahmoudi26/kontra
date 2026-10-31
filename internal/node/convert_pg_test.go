package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
)

// spec/tunnel.md §6.3 (D278) with the real admission: a node that served an anonymous session, restarted once kontra
// login stored an account's API key for the gate (here written as login writes it), sends that key as auth and no
// anonymous field, and its session converts: the welcome is the account's, and kontra connect prints the account's
// URLs, without the anonymous session's end or its public note.
func TestRealAdmissionConvertsAfterLogin(t *testing.T) {
	lim := anon.Defaults
	rg := newRealGateWith(t, func(o *enroll.Options) { o.Multi = true; o.Anonymous = &lim; o.EnrollWithAPIKey = true })
	ctx := context.Background()
	account := uuid.Must(uuid.NewV7())
	if _, err := rg.pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind) values ($1, 'acme', 'account')`, account); err != nil {
		t.Fatal(err)
	}
	key, err := rg.keys.Mint(ctx, auth.MintRequest{TenantID: account, Name: "login", Scopes: []auth.Scope{auth.ScopeHire, auth.ScopeEnrollAll}})
	if err != nil {
		t.Fatal(err)
	}
	rec := newRecorder(t)
	home := t.TempDir()
	cfg := testConfig(t, rg.url, home, a2aServe("site", rec.url()))
	cfg.Public = []string{"site"}
	tn := mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); !r.Hello.Anonymous {
		t.Fatal("the first hello did not ask for an anonymous session")
	}
	s := rg.session(t)
	tn.welcomed(t)
	slug := s.Tenant
	if !anon.IsSlug(slug) || !strings.Contains(tn.out.String(), "/agents/"+slug+"/site") {
		t.Fatalf("the anonymous session: %q\n%s", slug, tn.out.String())
	}
	tn.stop()
	_ = tn.result(t, 20*time.Second)

	// kontra login's store: the key under the gate's origin.
	origin, err := tunnel.CanonicalOrigin(rg.url)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := LoadCredentials(cfg.CredentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Update(origin, func(c *Credential) {
		c.APIKey = key.Secret
		c.OAuthClient = &OAuthClient{Issuer: "https://login.example", ClientID: "client-1"}
	}); err != nil {
		t.Fatal(err)
	}

	rg.drain()
	tn2 := mustNode(t, rg.testGate, testConfig(t, rg.url, home, a2aServe("site", rec.url()))).run(t)
	if r := rg.next(t); r.Hello.Anonymous || r.Hello.Auth != key.Secret || r.Hello.KID != s.KeyID {
		t.Fatalf("the hello after login: anonymous %v, auth set %v, key %s (was %s)", r.Hello.Anonymous, r.Hello.Auth != "", r.Hello.KID, s.KeyID)
	}
	s2 := rg.session(t)
	tn2.welcomed(t)
	if _, ok := s2.Anonymous(); ok || s2.Tenant != "acme" || s2.TenantID != account {
		t.Fatalf("the converted session: tenant %s, anonymous", s2.Tenant)
	}
	out := tn2.out.String()
	if !strings.Contains(out, "(tenant acme,") || !strings.Contains(out, "/agents/acme/site") || strings.Contains(out, "anonymous session") ||
		strings.Contains(out, "public: anyone") || strings.Contains(out, slug) {
		t.Fatalf("stdout after the conversion:\n%s", out)
	}
	if n := rg.count(t, `select count(*) from kontra.tenants where slug = $1 and expires_at <= now()`, slug); n != 1 {
		t.Error("the anonymous tenant did not end")
	}
}
