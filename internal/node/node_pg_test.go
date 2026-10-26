package node

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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
	team  uuid.UUID

	mu    sync.Mutex
	lose  func(n int, req enroll.Request) (commit, lose bool)
	reqs  []enroll.Request
	seen  chan enroll.Request
	codes []tunnel.Code
}

func newRealGate(t *testing.T, services ...string) *realGate {
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
	rg := &realGate{pool: pool, team: team, seen: make(chan enroll.Request, 64)}
	rg.testGate = newTestGate(t, func(c *tunnelgate.HandlerConfig) {
		rg.store = enroll.NewStore(pool, enroll.Options{PublicURL: c.PublicURL})
		c.Admitter = rg
	})
	return rg
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
