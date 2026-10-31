package node

import (
	"slices"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
)

// newAnonymousGate is the real admission of a multi-tenant gate with tunnel.anonymous (lim).
func newAnonymousGate(t *testing.T, lim anon.Limits, services ...string) *realGate {
	t.Helper()
	return newRealGateWith(t, func(o *enroll.Options) { o.Multi = true; o.Anonymous = &lim }, services...)
}

// D285 (the M4 audit's confirmed conflict on §2.4 and §6): on a gate that opens anonymous sessions, the probe after
// an enrollment that did not commit carries no anonymous field, so the real admission still answers it unknown_key,
// and the node sends its token again: the key is enrolled for the team's service, no anonymous tenant is opened, and
// the token is recorded consumed only by the welcome that enrolled it.
func TestRealAdmissionProbeOnAnAnonymousGate(t *testing.T) {
	rg := newAnonymousGate(t, anon.Defaults, "hello")
	rec := newRecorder(t)
	tok := rg.token(t, "hello")
	rg.setLose(func(n int, _ enroll.Request) (bool, bool) { return false, n == 0 }) // the first hello: not committed
	cfg := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
	cfg.Enroll = []string{tok}
	tn := mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{tok}) || r.Hello.Anonymous {
		t.Fatalf("first hello: enroll %v, anonymous %v", r.Hello.Enroll, r.Hello.Anonymous)
	}
	if r := rg.next(t); len(r.Hello.Enroll) != 0 || r.Hello.Anonymous || r.Hello.Auth != "" {
		t.Fatalf("the probe: enroll %v, anonymous %v", r.Hello.Enroll, r.Hello.Anonymous)
	}
	if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{tok}) || r.Hello.Anonymous {
		t.Fatalf("after unknown_key: enroll %v, anonymous %v", r.Hello.Enroll, r.Hello.Anonymous)
	}
	s := rg.session(t)
	if got := rg.answered(); !slices.Equal(got, []tunnel.Code{tunnel.CodeUnknownKey, ""}) {
		t.Fatalf("the real admission answered %v", got)
	}
	if s.TenantID != rg.team {
		t.Fatalf("the session's tenant %s (%s), want the team's", s.TenantID, s.Tenant)
	}
	if _, ok := s.Anonymous(); ok {
		t.Fatal("an anonymous session")
	}
	if n := rg.count(t, `select count(*) from kontra.tenants where kind = 'anonymous'`); n != 0 {
		t.Fatalf("%d anonymous tenants opened", n)
	}
	eventually(t, 5*time.Second, "the token recorded consumed", func() bool {
		c := tn.creds(t)
		return len(c.Enroll) == 0 && len(c.Unconfirmed) == 0 && slices.Equal(c.Consumed, []string{TokenHash(tok)})
	})
}

// §6, §6.1 (D285) with the real admission: a node holding no credential for the gate asks for an anonymous session
// and is welcomed under anon-<16 hex>, its --public service public; a node of the same host given an --enroll token
// does not ask, and is enrolled in the team as §5 says.
func TestRealAdmissionOpensAnAnonymousSession(t *testing.T) {
	rg := newAnonymousGate(t, anon.Defaults, "hello")
	rec := newRecorder(t)
	cfg := testConfig(t, rg.url, "", a2aServe("site", rec.url()))
	cfg.Public = []string{"site"}
	tn := mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); !r.Hello.Anonymous || !slices.Equal(r.Hello.Public, []string{"site"}) {
		t.Fatalf("hello: anonymous %v, public %v", r.Hello.Anonymous, r.Hello.Public)
	}
	s := rg.session(t)
	tn.welcomed(t)
	if !anon.IsSlug(s.Tenant) {
		t.Fatalf("tenant %q", s.Tenant)
	}
	if _, ok := s.Anonymous(); !ok {
		t.Fatal("not an anonymous session")
	}
	out := tn.out.String()
	if !strings.Contains(out, "/agents/"+s.Tenant+"/site  (public: anyone may call it)") || !strings.Contains(out, "anonymous session: it ends at") {
		t.Fatalf("stdout:\n%s", out)
	}
	if resp, _ := exchange(t, s, "site", "/x"); resp.StatusCode != 200 {
		t.Fatalf("the session does not serve: %d", resp.StatusCode)
	}
	var allow []string
	if err := rg.pool.QueryRow(t.Context(), `select allow from kontra.services where tenant_id = $1 and name = 'site'`, s.TenantID).Scan(&allow); err != nil ||
		!slices.Contains(allow, "anonymous") {
		t.Fatalf("allow %v %v", allow, err)
	}

	cfg2 := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
	cfg2.Enroll = []string{rg.token(t, "hello")}
	mustNode(t, rg.testGate, cfg2).run(t)
	if r := rg.next(t); r.Hello.Anonymous {
		t.Fatal("a node given --enroll asked for an anonymous session")
	}
	if s2 := rg.session(t); s2.TenantID != rg.team {
		t.Fatalf("the enrolled node's tenant %s", s2.Tenant)
	}
}
