package tunnelgate

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
)

// The Handler with the gate's own Admitter (enroll.Store on Postgres): a service token enrolls the key it
// was proven for; the consumed token is never accepted again (enroll_invalid); the enrolled key needs
// nothing but the proof afterwards; a rotation proves both keys and commits (§4, §5, §12).
func TestHandlerWithEnrollStore(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	team, err := tenant.EnsureTeam(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, source)
		values ($1, 'hello', 'a2a', '{"tunnel": {}}'::jsonb, 'config')`, team); err != nil {
		t.Fatal(err)
	}
	// The store's public_url is the gate's, known once its server listens.
	var sp atomic.Pointer[enroll.Store]
	g := newTestGate(t, gateOpts{admitter: admitterFunc(func(ctx context.Context, req enroll.Request) (*enroll.Decision, error) {
		return sp.Load().Admit(ctx, req)
	})})
	store := enroll.NewStore(pool, enroll.Options{PublicURL: g.gateURL.String()})
	sp.Store(store)
	tok, err := store.MintServiceToken(ctx, team, "hello", "test")
	if err != nil {
		t.Fatal(err)
	}

	id := newIdentity(t)
	hello := newHello("hello")
	hello.Enroll = []string{tok}
	d, w, err := g.connect(t, id, hello, false)
	if err != nil {
		t.Fatalf("enrolling handshake: %v", err)
	}
	if w.Tenant != tenant.TeamSlug || len(w.Services) != 1 || w.Services[0].URL != g.gateURL.String()+"/agents/hello" {
		t.Fatalf("welcome %+v", w)
	}
	// The node runs yamux, so that the gate's control stream opens and the session is registered.
	m, err := tunnel.ClientSession(ctx, d.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := g.sink.next(t)
	_ = m.Close()
	if s.KeyID != id.KeyID() || s.TenantID != team || s.HirePrincipal != "node:"+id.KeyID() {
		t.Fatalf("session %+v", s)
	}
	var pub []byte
	if err := pool.QueryRow(ctx, `select pubkey from kontra.tunnel_keys where key_id = $1`, id.KeyID()).Scan(&pub); err != nil {
		t.Fatal(err)
	}
	want, _ := tunnel.ParseRecipient(id.Recipient())
	if !bytes.Equal(pub, want.Bytes()) {
		t.Fatalf("stored public key %x, want the proven one", pub)
	}
	var bound int
	if err := pool.QueryRow(ctx, `select count(*) from kontra.key_services where key_id = $1 and revoked_at is null`,
		id.KeyID()).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("%d bindings, %v", bound, err)
	}

	// The consumed token is refused even for the key it enrolled; another key with it too.
	for _, who := range []*nodekey.Identity{id, newIdentity(t)} {
		h := newHello("hello")
		h.Enroll = []string{tok}
		_, _, err := g.connect(t, who, h, false)
		if code := handshakeCode(t, err); code != tunnel.CodeEnrollInvalid {
			t.Fatalf("a consumed token: %s, want enroll_invalid", code)
		}
	}
	// The enrolled key needs nothing but its proof (§5).
	d, _, err = g.connect(t, id, newHello("hello"), false)
	if err != nil {
		t.Fatalf("reconnect without a token: %v", err)
	}
	_ = d.WS.CloseNow()
	// An unknown key without an authority is unknown_key, which the node retries.
	_, _, err = g.connect(t, newIdentity(t), newHello("hello"), false)
	if code := handshakeCode(t, err); code != tunnel.CodeUnknownKey || !code.Retryable() {
		t.Fatalf("unknown key: %s", code)
	}

	// Rotation (§12): the new key proves both, and the old one is marked rotated to it.
	nid, err := nodekey.Rotate(id.Path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := newHello("hello")
	h.RotateFrom = &tunnel.KeyRef{KID: id.KeyID(), Recipient: id.Recipient()}
	d, w, err = g.connect(t, nid, h, false)
	if err != nil {
		t.Fatalf("rotation: %v", err)
	}
	_ = d.WS.CloseNow()
	if len(w.Services) != 1 {
		t.Fatalf("rotated welcome %+v", w)
	}
	var rotatedTo string
	if err := pool.QueryRow(ctx, `select coalesce(rotated_to, '') from kontra.tunnel_keys where key_id = $1`,
		id.KeyID()).Scan(&rotatedTo); err != nil || rotatedTo != nid.KeyID() {
		t.Fatalf("old key rotated_to %q, %v; want %s", rotatedTo, err, nid.KeyID())
	}
}

type admitterFunc func(context.Context, enroll.Request) (*enroll.Decision, error)

func (f admitterFunc) Admit(ctx context.Context, req enroll.Request) (*enroll.Decision, error) {
	return f(ctx, req)
}
