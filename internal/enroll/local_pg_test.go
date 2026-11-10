package enroll

import (
	"testing"

	"kontra.sh/kontra/internal/tunnel"
)

// The local development profile (Options.Local): a node with no token and no credential (its hello asks for an
// anonymous session, as kontra connect's does) is enrolled on the team tenant for every service it declares; missing
// ones are created as tunnel services with allow [anonymous]; a configured service is served with its own settings;
// another key may serve a service that already has one; a node that declares nothing is admitted to call agents.
func TestLocalProfileEnrollsWithoutCredentials(t *testing.T) {
	e := newPGEnv(t, func(o *Options) { o.Local, o.PublicURL = true, "http://127.0.0.1:8080" })
	e.service(e.team, "configured", "a2a", false)
	e.exec(`update kontra.services set allow = '{key:kk_a_*}', retention_s = 60 where name = 'configured'`)

	k := newTestKey(t)
	h := hello(k, a2a("hello"), mcp("tools"), a2a("configured"))
	h.Anonymous = true
	d := e.mustAdmit(k, h)
	if got := served(d); len(got) != 3 || got[0] != "a2a:hello@http://127.0.0.1:8080/agents/hello" {
		t.Fatalf("served %v", got)
	}
	if d.Key.TenantID != e.team || d.Key.Tenant != "default" {
		t.Fatalf("key tenant %s %s, want the team tenant", d.Key.Tenant, d.Key.TenantID)
	}
	if n := e.count(`select count(*) from kontra.services where tenant_id = $1 and name in ('hello', 'tools')
		and backend ? 'tunnel' and allow = '{anonymous}' and disabled_at is null`, e.team); n != 2 {
		t.Fatalf("created services: %d, want 2 tunnel services with allow [anonymous]", n)
	}
	if n := e.count(`select count(*) from kontra.services where name = 'configured' and allow = '{key:kk_a_*}' and retention_s = 60`); n != 1 {
		t.Fatal("the configured service's settings changed")
	}
	if n := e.count(`select count(*) from kontra.tenants where kind = 'anonymous'`); n != 0 {
		t.Fatalf("%d anonymous tenants, want none", n)
	}

	// The same key again (a restart of kontra connect), and a second key for the same service.
	e.mustAdmit(k, hello(k, a2a("hello")))
	k2 := newTestKey(t)
	if got := served(e.mustAdmit(k2, hello(k2, a2a("hello")))); len(got) != 1 {
		t.Fatalf("a second key: served %v", got)
	}

	// A kind mismatch is still refused.
	k3 := newTestKey(t)
	_, err := e.admit(k3, hello(k3, mcp("hello")), nil)
	wantCode(t, "mcp:hello over an a2a service", err, tunnel.CodeKindMismatch)

	// A node that only calls agents.
	k4 := newTestKey(t)
	if d := e.mustAdmit(k4, hello(k4)); len(d.Served) != 0 || d.HirePrincipal != "node:"+k4.kid {
		t.Fatalf("hire-only: %+v", d)
	}
	e.verifyAudit()
}

// Without Local nothing changes: the same hello is anonymous_unavailable.
func TestNoLocalProfileStillRefuses(t *testing.T) {
	e := newPGEnv(t)
	k := newTestKey(t)
	h := hello(k, a2a("hello"))
	h.Anonymous = true
	_, err := e.admit(k, h, nil)
	wantCode(t, "anonymous hello on a team gate", err, tunnel.CodeAnonymousUnavailable)
}
