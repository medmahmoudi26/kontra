package tunnelgate

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
)

// spec/tunnel.md §6.1, §12 (D277, D287) with the real admission and registry: an anonymous session's tenant
// ended by the sweep (here before the session's own clock, as a sweep on a replica whose clock runs ahead would)
// reaches the replica holding its session by NOTIFY kontra_tunnel: the node is told {"t": "revoked"} once, never
// {"t": "revoked", "service"} for the bindings revoked with it, and the session closes and its tunnels row goes; the
// key is not revoked, and its next hello opens a new tenant.
func TestAnonymousTenantEndClosesItsSession(t *testing.T) {
	e := newTunnelEnv(t)
	lim := anon.Defaults
	e.store = enroll.NewStore(e.pool, enroll.Options{PublicURL: "http://gate.test", Multi: true, Anonymous: &lim})
	r := e.replica("a", replicaOpts{})
	id := newIdentity(t)
	h := newHello("hello", "other")
	h.Anonymous = true
	n, err := r.startNode(t, id, h, newAgent())
	if err != nil {
		t.Fatal(err)
	}
	e.waitRow(t, n.session)
	if n.welcome.Anonymous == nil || !anon.IsSlug(n.welcome.Tenant) {
		t.Fatalf("welcome %+v", n.welcome)
	}
	var tenantID uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `select id from kontra.tenants where slug = $1`, n.welcome.Tenant).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `update kontra.tenants set expires_at = now() - interval '1 second' where id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if ended, err := anon.NewSweeper(e.pool, lim, nil, nil).Expire(context.Background()); err != nil || ended != 1 {
		t.Fatalf("expire: %d %v", ended, err)
	}
	if c := n.nextControl(t, 10*time.Second); c.T != tunnel.CtlRevoked || c.Service != "" {
		t.Fatalf("control %+v, want revoked", c)
	}
	if !n.closed(10 * time.Second) {
		t.Fatal("the session did not close")
	}
	n.noControl(t, 500*time.Millisecond)
	eventually(t, 10*time.Second, "the tunnels row deleted", func() bool { return !e.hasRow(t, n.session) })
	var revoked bool
	if err := e.pool.QueryRow(context.Background(), `select revoked_at is not null from kontra.tunnel_keys where key_id = $1`, id.KeyID()).Scan(&revoked); err != nil || revoked {
		t.Fatalf("the key revoked %v %v", revoked, err)
	}
	n2, err := r.startNode(t, id, h, newAgent())
	if err != nil {
		t.Fatal(err)
	}
	if n2.welcome.Tenant == n.welcome.Tenant || !anon.IsSlug(n2.welcome.Tenant) || n2.welcome.Anonymous == nil {
		t.Fatalf("the next welcome %+v (the first's tenant %s)", n2.welcome, n.welcome.Tenant)
	}
}
