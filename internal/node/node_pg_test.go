package node

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
)

// The lost-welcome protocol (M2 plan audit gap 1, conflict C2) against the gate's real admission
// (enroll.Store on Postgres, WP-02): what the probe's answers mean there is what the node relies on.

// realGate is a migrated database with the team tenant and tunnel services, and an in-process Handler
// whose Admitter is the real enroll.Store behind a switch that can lose the outcome of an admission: commit
// it and then fail as the gate's own error (the welcome lost after the commit), or fail before it.
type realGate struct {
	*testGate
	pool  *pgxpool.Pool
	store *enroll.Store
	keys  *auth.APIKeys // authenticates hello's auth
	team  uuid.UUID

	mu    sync.Mutex
	lose  func(n int, req enroll.Request) (commit, lose bool)
	reqs  []enroll.Request
	seen  chan enroll.Request
	codes []tunnel.Code
}

func newRealGate(t *testing.T, services ...string) *realGate {
	t.Helper()
	return newRealGateWith(t, nil, services...)
}

// newRealGateWith is newRealGate with the admission's options changed by mod.
func newRealGateWith(t *testing.T, mod func(*enroll.Options), services ...string) *realGate {
	t.Helper()
	pool := pgtest.Pool(t, pgtest.Migrated(t))
	team, err := tenant.EnsureTeam(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range services {
		if _, err := pool.Exec(context.Background(),
			`insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, $2, 'a2a', '{"tunnel": {}}'::jsonb, 'config')`,
			team, s); err != nil {
			t.Fatal(err)
		}
	}
	rg := &realGate{pool: pool, team: team, keys: auth.NewAPIKeys(pool, nil), seen: make(chan enroll.Request, 64)}
	t.Cleanup(rg.keys.Wait)
	rg.testGate = newTestGate(t, func(c *tunnelgate.HandlerConfig) {
		o := enroll.Options{PublicURL: c.PublicURL, Keys: rg.keys}
		if mod != nil {
			mod(&o)
		}
		rg.store = enroll.NewStore(pool, o)
		c.Admitter = rg
	})
	return rg
}

// drain forgets the hellos not read yet (those of a node the test stopped).
func (rg *realGate) drain() {
	for {
		select {
		case <-rg.seen:
		default:
			return
		}
	}
}

// last is the last n codes the real admission answered.
func (rg *realGate) last(n int) []tunnel.Code {
	got := rg.answered()
	return got[max(0, len(got)-n):]
}

func (rg *realGate) Admit(ctx context.Context, req enroll.Request) (*enroll.Decision, error) {
	rg.mu.Lock()
	n := len(rg.reqs)
	rg.reqs = append(rg.reqs, req)
	lose := rg.lose
	rg.mu.Unlock()
	rg.seen <- req
	if lose != nil {
		if commit, lost := lose(n, req); lost {
			if commit {
				if _, err := rg.store.Admit(ctx, req); err != nil {
					return nil, err
				}
			}
			return nil, gateFails
		}
	}
	d, err := rg.store.Admit(ctx, req)
	rg.mu.Lock()
	rg.codes = append(rg.codes, handshakeCode(err))
	rg.mu.Unlock()
	return d, err
}

func (rg *realGate) setLose(f func(n int, req enroll.Request) (commit, lose bool)) {
	rg.mu.Lock()
	rg.lose = f
	rg.mu.Unlock()
}

func (rg *realGate) next(t *testing.T) enroll.Request {
	t.Helper()
	select {
	case r := <-rg.seen:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("no hello reached Admit")
		return enroll.Request{}
	}
}

// answered is the real admission's codes so far ("" for a welcome), the lost ones left out.
func (rg *realGate) answered() []tunnel.Code {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return slices.Clone(rg.codes)
}

func (rg *realGate) token(t *testing.T, service string) string {
	t.Helper()
	tok, err := rg.store.MintServiceToken(context.Background(), rg.team, service, "test")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (rg *realGate) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := rg.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An enrollment whose welcome was lost after the gate committed it: the probe is welcomed by the real
// admission (the key is now enrolled), the token is recorded consumed, and a restart with the same --enroll
// is welcomed without sending it. Without the probe, the second hello would carry a used token and be
// refused enroll_invalid, which the node does not retry.
func TestRealAdmissionLostWelcome(t *testing.T) {
	for _, committed := range []bool{true, false} {
		name := "not committed"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			rg := newRealGate(t, "hello")
			rec := newRecorder(t)
			tok := rg.token(t, "hello")
			rg.setLose(func(n int, _ enroll.Request) (bool, bool) { return committed, n == 0 })
			cfg := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
			cfg.Enroll = []string{tok}
			tn := mustNode(t, rg.testGate, cfg).run(t)
			if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{tok}) {
				t.Fatalf("first hello: %v", r.Hello.Enroll)
			}
			if r := rg.next(t); len(r.Hello.Enroll) != 0 {
				t.Fatalf("the probe carried %v", r.Hello.Enroll)
			}
			want := []tunnel.Code{""}
			if !committed {
				// The real admission answers the probe of an unknown key unknown_key (WP-02), and the full hello
				// follows.
				if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{tok}) {
					t.Fatalf("after unknown_key: %v", r.Hello.Enroll)
				}
				want = []tunnel.Code{tunnel.CodeUnknownKey, ""}
			}
			s := rg.session(t)
			if got := rg.answered(); !slices.Equal(got, want) {
				t.Fatalf("the real admission answered %v, want %v", got, want)
			}
			if resp, _ := exchange(t, s, "hello", "/x"); resp.StatusCode != 200 {
				t.Fatalf("the session does not serve: %d", resp.StatusCode)
			}
			eventually(t, 5*time.Second, "the token recorded consumed", func() bool {
				c := tn.creds(t)
				return len(c.Enroll) == 0 && len(c.Unconfirmed) == 0 && slices.Equal(c.Consumed, []string{TokenHash(tok)})
			})
			if n := rg.count(t, `select count(*) from kontra.key_services where key_id = $1 and service = 'hello' and revoked_at is null`,
				tn.n.cfgKeyID(t)); n != 1 {
				t.Fatalf("%d bindings", n)
			}
			tn.stop()
			tn.result(t, 10*time.Second)

			// A restart with the same flag (a compose sidecar's restart).
			mustNode(t, rg.testGate, cfg).run(t)
			if r := rg.next(t); len(r.Hello.Enroll) != 0 {
				t.Fatalf("the restart sent the used token: %v", r.Hello.Enroll)
			}
			rg.session(t)
			if got := rg.answered(); got[len(got)-1] != "" {
				t.Fatalf("the restart was answered %v", got)
			}
		})
	}
}

// A known key adding a second service with a token, whose welcome was lost before the commit: the real
// admission answers the probe service_denied (the key is not enrolled for the new service), and the node
// sends the full hello, which is welcomed.
func TestRealAdmissionLostWelcomeKnownKey(t *testing.T) {
	rg := newRealGate(t, "hello", "other")
	rec := newRecorder(t)
	cfg := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
	cfg.Enroll = []string{rg.token(t, "hello")}
	tn := mustNode(t, rg.testGate, cfg).run(t)
	rg.next(t)
	rg.session(t)
	tn.welcomed(t)
	tn.stop()
	tn.result(t, 10*time.Second)

	tok := rg.token(t, "other")
	cfg.Serves = append(cfg.Serves, a2aServe("other", rec.url()))
	cfg.Enroll = []string{tok}
	rg.setLose(func(n int, _ enroll.Request) (bool, bool) { return false, n == 1 })
	tn = mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{tok}) {
		t.Fatalf("hello: %v", r.Hello.Enroll)
	}
	if r := rg.next(t); len(r.Hello.Enroll) != 0 {
		t.Fatalf("probe: %v", r.Hello.Enroll)
	}
	if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{tok}) {
		t.Fatalf("after service_denied: %v", r.Hello.Enroll)
	}
	s := rg.session(t)
	if got := rg.answered(); !slices.Equal(got, []tunnel.Code{"", tunnel.CodeServiceDenied, ""}) {
		t.Fatalf("the real admission answered %v", got)
	}
	if resp, _ := exchange(t, s, "other", "/x"); resp.StatusCode != 200 {
		t.Fatalf("the new service is not served: %d", resp.StatusCode)
	}
	eventually(t, 5*time.Second, "consumed", func() bool { return slices.Contains(tn.creds(t).Consumed, TokenHash(tok)) })
}

// A rotation whose welcome was lost after the gate committed it (C2): the real admission would refuse the
// same rotate_from enroll_invalid; the probe with the new key alone is welcomed, the rotation is confirmed
// in the key file, and the gate's old key points at the new one.
func TestRealAdmissionLostRotationWelcome(t *testing.T) {
	for _, committed := range []bool{true, false} {
		name := "not committed"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			rg := newRealGate(t, "hello")
			rec := newRecorder(t)
			cfg := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
			cfg.Enroll = []string{rg.token(t, "hello")}
			tn := mustNode(t, rg.testGate, cfg).run(t)
			old := rg.next(t).Hello
			rg.session(t)
			tn.welcomed(t)
			tn.stop()
			tn.result(t, 10*time.Second)

			cfg.Enroll = nil
			cfg.Rotate = true
			rg.setLose(func(_ int, req enroll.Request) (bool, bool) {
				return committed, req.Hello.RotateFrom != nil && len(rg.answered()) == 1
			})
			tn = mustNode(t, rg.testGate, cfg).run(t)
			if r := rg.next(t); r.Hello.RotateFrom == nil || r.Hello.RotateFrom.KID != old.KID {
				t.Fatalf("the rotation's hello: %+v", r.Hello.RotateFrom)
			}
			probe := rg.next(t)
			if probe.Hello.RotateFrom != nil || probe.Hello.KID == old.KID {
				t.Fatalf("the probe: kid %s rotate_from %+v", probe.Hello.KID, probe.Hello.RotateFrom)
			}
			want := []tunnel.Code{"", ""}
			if !committed {
				if r := rg.next(t); r.Hello.RotateFrom == nil {
					t.Fatal("after unknown_key the rotation was not sent again")
				}
				want = []tunnel.Code{"", tunnel.CodeUnknownKey, ""}
			}
			rg.session(t)
			if got := rg.answered(); !slices.Equal(got, want) {
				t.Fatalf("the real admission answered %v, want %v", got, want)
			}
			if n := rg.count(t, `select count(*) from kontra.tunnel_keys where key_id = $1 and rotated_to = $2`, old.KID, probe.Hello.KID); n != 1 {
				t.Fatal("the gate's old key does not point at the new one")
			}
			origin, _ := tunnel.CanonicalOrigin(rg.url)
			eventually(t, 5*time.Second, "the rotation confirmed in the key file", func() bool {
				id, err := nodekey.Load(cfg.KeyPath)
				if err != nil {
					return false
				}
				_, pending := id.PendingRotation(origin)
				return !pending
			})
		})
	}
}

// A node that rotated at another gate (that gate confirmed it) enrolls with --enroll at a gate that never
// knew its old key (§2.1, §5): this gate refuses rotate_from enroll_invalid ("no key of this gate"), the
// node sends the hello again without it, and the token enrolls the new key. The rotation is then settled at
// this gate: a reconnect sends neither rotate_from nor the consumed token (M2 WP-06 review: rotate_from was
// sent to every gate that had not confirmed the rotation, and its refusal was final).
func TestRealAdmissionRotationAtANewGate(t *testing.T) {
	rg := newRealGate(t, "hello")
	rec := newRecorder(t)
	cfg := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
	now := time.Now()
	old, _, err := nodekey.LoadOrCreate(cfg.KeyPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodekey.Rotate(cfg.KeyPath, now); err != nil {
		t.Fatal(err)
	}
	if err := nodekey.MarkRotated(cfg.KeyPath, old.Recipient(), "https://g1.example", now); err != nil {
		t.Fatal(err)
	}
	tok := rg.token(t, "hello")
	cfg.Enroll = []string{tok}
	tn := mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); r.Hello.RotateFrom != nil || len(r.Hello.Enroll) != 0 || r.Hello.Auth != "" {
		t.Fatalf("the first hello is not the probe: %+v", r.Hello)
	}
	if r := rg.next(t); r.Hello.RotateFrom == nil || r.Hello.RotateFrom.KID != old.KeyID() || !slices.Equal(r.Hello.Enroll, []string{tok}) {
		t.Fatalf("the full hello: rotate_from %+v enroll %v", r.Hello.RotateFrom, r.Hello.Enroll)
	}
	if r := rg.next(t); r.Hello.RotateFrom != nil || !slices.Equal(r.Hello.Enroll, []string{tok}) {
		t.Fatalf("after the refusal of rotate_from: rotate_from %+v enroll %v", r.Hello.RotateFrom, r.Hello.Enroll)
	}
	s := rg.session(t)
	if got, want := rg.answered(), []tunnel.Code{tunnel.CodeUnknownKey, tunnel.CodeEnrollInvalid, ""}; !slices.Equal(got, want) {
		t.Fatalf("the real admission answered %v, want %v", got, want)
	}
	if resp, _ := exchange(t, s, "hello", "/x"); resp.StatusCode != 200 {
		t.Fatalf("the session does not serve: %d", resp.StatusCode)
	}
	origin, _ := tunnel.CanonicalOrigin(rg.url)
	eventually(t, 5*time.Second, "the token consumed and the rotation settled at this gate", func() bool {
		id, err := nodekey.Load(cfg.KeyPath)
		if err != nil {
			return false
		}
		_, pending := id.PendingRotation(origin)
		return !pending && slices.Equal(tn.creds(t).Consumed, []string{TokenHash(tok)})
	})
	for _, w := range rg.wires.All() {
		_ = w.Cut()
	}
	if r := rg.next(t); r.Hello.RotateFrom != nil || len(r.Hello.Enroll) != 0 {
		t.Fatalf("the reconnect: rotate_from %+v enroll %v", r.Hello.RotateFrom, r.Hello.Enroll)
	}
	rg.session(t)
	if got := rg.last(1); got[0] != "" {
		t.Fatalf("the reconnect was answered %v", got)
	}
}

// Rotate after a leak, then revoke the old key (§12, §15), the revocation landing before the node's
// rotation hello: the gate refuses rotate_from revoked, and the node sends the hello again without it.
// Without a token that is unknown_key, which the node retries (its operator must enroll the new key), not
// a final refusal; with a fresh --enroll the token enrolls the new key; and a restart without it is
// welcomed (M2 WP-06 review: every restart was refused revoked, even with a fresh token).
func TestRealAdmissionRotationAfterOldKeyRevoked(t *testing.T) {
	rg := newRealGate(t, "hello")
	rec := newRecorder(t)
	cfg := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
	cfg.Enroll = []string{rg.token(t, "hello")}
	tn := mustNode(t, rg.testGate, cfg).run(t)
	rg.next(t)
	rg.session(t)
	tn.welcomed(t)
	tn.stop()
	tn.result(t, 10*time.Second)
	oldKID := tn.n.cfgKeyID(t)
	if _, err := nodekey.Rotate(cfg.KeyPath, time.Now()); err != nil { // kontra connect --rotate, offline
		t.Fatal(err)
	}
	if err := rg.store.RevokeKey(context.Background(), oldKID, "operator", "test"); err != nil {
		t.Fatal(err)
	}

	// No token: the probe, rotate_from (revoked), then the hello without it (unknown_key, retried).
	cfg.Enroll = nil
	tn = mustNode(t, rg.testGate, cfg)
	tn.n.backoff = Backoff{rand: func() float64 { return 0.5 }}
	tn.run(t)
	if r := rg.next(t); r.Hello.RotateFrom != nil {
		t.Fatalf("the probe carried rotate_from")
	}
	if r := rg.next(t); r.Hello.RotateFrom == nil || r.Hello.RotateFrom.KID != oldKID {
		t.Fatalf("the full hello: rotate_from %+v", r.Hello.RotateFrom)
	}
	if r := rg.next(t); r.Hello.RotateFrom != nil {
		t.Fatalf("rotate_from was sent again after its refusal")
	}
	eventually(t, 5*time.Second, "unknown_key for the hello without rotate_from", func() bool {
		return slices.Equal(rg.last(3), []tunnel.Code{tunnel.CodeUnknownKey, tunnel.CodeRevoked, tunnel.CodeUnknownKey})
	})
	time.Sleep(200 * time.Millisecond)
	if !tn.running() {
		t.Fatalf("Run ended: %v", tn.result(t, time.Second))
	}
	tn.stop()
	tn.result(t, 10*time.Second)
	rg.drain()

	// A fresh token enrolls the new key.
	tok := rg.token(t, "hello")
	cfg.Enroll = []string{tok}
	tn = mustNode(t, rg.testGate, cfg).run(t)
	rg.next(t) // the probe
	if r := rg.next(t); r.Hello.RotateFrom == nil || !slices.Equal(r.Hello.Enroll, []string{tok}) {
		t.Fatalf("the full hello: rotate_from %+v enroll %v", r.Hello.RotateFrom, r.Hello.Enroll)
	}
	if r := rg.next(t); r.Hello.RotateFrom != nil || !slices.Equal(r.Hello.Enroll, []string{tok}) {
		t.Fatalf("after the refusal of rotate_from: rotate_from %+v enroll %v", r.Hello.RotateFrom, r.Hello.Enroll)
	}
	s := rg.session(t)
	if got, want := rg.last(3), []tunnel.Code{tunnel.CodeUnknownKey, tunnel.CodeRevoked, ""}; !slices.Equal(got, want) {
		t.Fatalf("the real admission answered %v, want %v", got, want)
	}
	if resp, _ := exchange(t, s, "hello", "/x"); resp.StatusCode != 200 {
		t.Fatalf("the session does not serve: %d", resp.StatusCode)
	}
	eventually(t, 5*time.Second, "the token consumed", func() bool {
		c := tn.creds(t)
		return slices.Contains(c.Consumed, TokenHash(tok)) && len(c.Enroll) == 0 && len(c.Unconfirmed) == 0
	})
	tn.stop()
	tn.result(t, 10*time.Second)
	rg.drain()

	// A restart without the token: one hello, without rotate_from, welcomed.
	cfg.Enroll = nil
	mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); r.Hello.RotateFrom != nil || len(r.Hello.Enroll) != 0 {
		t.Fatalf("the restart: rotate_from %+v enroll %v", r.Hello.RotateFrom, r.Hello.Enroll)
	}
	rg.session(t)
	if got := rg.last(1); got[0] != "" {
		t.Fatalf("the restart was answered %v", got)
	}
}

// A token that re-enrolls a revoked binding (a one-time token does, WP-02), whose hello lost its outcome
// before the commit: the probe is welcomed without the service (§5: a revoked binding is left out), which
// is not taken for the enrollment's commit. The full hello follows, and the token re-enrolls the binding
// (M2 WP-06 review: the token was recorded consumed, unused, and the session served nothing).
func TestRealAdmissionProbeOnRevokedBinding(t *testing.T) {
	rg := newRealGate(t, "hello")
	rec := newRecorder(t)
	cfg := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
	cfg.Enroll = []string{rg.token(t, "hello")}
	tn := mustNode(t, rg.testGate, cfg).run(t)
	rg.next(t)
	rg.session(t)
	tn.welcomed(t)
	tn.stop()
	tn.result(t, 10*time.Second)
	kid := tn.n.cfgKeyID(t)
	if err := rg.store.RevokeBinding(context.Background(), kid, "hello", "test"); err != nil {
		t.Fatal(err)
	}

	tok2 := rg.token(t, "hello")
	cfg.Enroll = []string{tok2}
	rg.setLose(func(n int, _ enroll.Request) (bool, bool) { return false, n == 1 }) // not committed, lost
	tn = mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{tok2}) {
		t.Fatalf("the hello: %v", r.Hello.Enroll)
	}
	if r := rg.next(t); len(r.Hello.Enroll) != 0 {
		t.Fatalf("the probe: %v", r.Hello.Enroll)
	}
	if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{tok2}) {
		t.Fatalf("after a probe welcomed without the service, the full hello must follow: %v", r.Hello.Enroll)
	}
	rg.session(t) // the probe's, closed by the node
	s := rg.session(t)
	if got := rg.answered(); !slices.Equal(got, []tunnel.Code{"", "", ""}) {
		t.Fatalf("the real admission answered %v", got)
	}
	if n := rg.count(t, `select count(*) from kontra.enrollment_tokens where used_at is not null`); n != 2 {
		t.Fatalf("%d tokens used at the gate", n)
	}
	if resp, _ := exchange(t, s, "hello", "/x"); resp.StatusCode != 200 {
		t.Fatalf("the re-enrolled service is not served: %d", resp.StatusCode)
	}
	eventually(t, 5*time.Second, "tok2 consumed", func() bool {
		c := tn.creds(t)
		return slices.Contains(c.Consumed, TokenHash(tok2)) && len(c.Enroll) == 0 && len(c.Unconfirmed) == 0
	})
}

// The probe carries no API key (M2 WP-06 review). On a gate with tunnel.enroll_with_api_key, the API key's
// enroll: scope would enroll the new key of a rotation that did not commit: the node took that welcome for
// the rotation's, stamped it, and would later prune an old key the gate still holds enrolled. Without it,
// the probe of an uncommitted rotation is unknown_key and the rotation goes again; the probe of a committed
// one is welcomed, and the node dials again with its API key, the session's hiring principal.
func TestRealAdmissionLostRotationWithAPIKey(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "not committed"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			rg := newRealGateWith(t, func(o *enroll.Options) { o.EnrollWithAPIKey = true }, "hello")
			key, err := rg.keys.Mint(context.Background(), auth.MintRequest{TenantID: rg.team, Name: "node",
				Scopes: []auth.Scope{auth.ScopeHire, auth.ScopeEnrollAll}})
			if err != nil {
				t.Fatal(err)
			}
			rec := newRecorder(t)
			cfg := testConfig(t, rg.url, "", a2aServe("hello", rec.url()))
			cfg.APIKey = key.Secret
			tn := mustNode(t, rg.testGate, cfg).run(t)
			old := rg.next(t).Hello // enrolled by the API key's enroll:*
			rg.session(t)
			tn.welcomed(t)
			tn.stop()
			tn.result(t, 10*time.Second)

			cfg.Rotate = true
			rg.setLose(func(_ int, req enroll.Request) (bool, bool) {
				return committed, req.Hello.RotateFrom != nil && len(rg.answered()) == 1
			})
			tn = mustNode(t, rg.testGate, cfg).run(t)
			if r := rg.next(t); r.Hello.RotateFrom == nil || r.Hello.Auth != key.Secret {
				t.Fatalf("the rotation's hello: rotate_from %+v auth %v", r.Hello.RotateFrom, r.Hello.Auth != "")
			}
			probe := rg.next(t)
			if probe.Hello.RotateFrom != nil || probe.Hello.Auth != "" || len(probe.Hello.Enroll) != 0 {
				t.Fatalf("the probe carried rotate_from %+v, auth %v, enroll %v", probe.Hello.RotateFrom, probe.Hello.Auth != "", probe.Hello.Enroll)
			}
			third := rg.next(t)
			if third.Hello.Auth != key.Secret || (third.Hello.RotateFrom != nil) == committed {
				t.Fatalf("after the probe: rotate_from %+v auth %v", third.Hello.RotateFrom, third.Hello.Auth != "")
			}
			want := []tunnel.Code{"", tunnel.CodeUnknownKey, ""}
			if committed {
				want = []tunnel.Code{"", "", ""}
				rg.session(t) // the probe's, closed by the node
			}
			s := rg.session(t)
			if got := rg.answered(); !slices.Equal(got, want) {
				t.Fatalf("the real admission answered %v, want %v", got, want)
			}
			if n := rg.count(t, `select count(*) from kontra.tunnel_keys where key_id = $1 and rotated_to = $2`, old.KID, probe.Hello.KID); n != 1 {
				t.Fatal("the gate's old key does not point at the new one: the rotation did not happen")
			}
			if resp, _ := exchange(t, s, "hello", "/x"); resp.StatusCode != 200 {
				t.Fatalf("the session does not serve: %d", resp.StatusCode)
			}
		})
	}
}
